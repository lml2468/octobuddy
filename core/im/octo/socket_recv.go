package octo

import (
	"encoding/json"
	"fmt"
	"strconv"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// settingByte bit layout (socket.ts parseSettingByte).
func settingTopic(v byte) bool    { return (v>>3)&0x01 == 1 }
func settingStreamOn(v byte) bool { return (v>>1)&0x01 == 1 }

// onRecv parses a RECV body, decrypts the payload, acks, and forwards.
func (s *socketConn) onRecv(body []byte) {
	d := &decoder{buf: body}
	setting, err := d.readByte()
	if err != nil {
		return
	}
	if _, err = d.readString(); err != nil { // msgKey (unused)
		return
	}
	fromUID, _ := d.readString()
	channelID, cerr := d.readString()
	channelType, _ := d.readByte()
	if s.srvVer >= 3 {
		_, _ = d.readInt32() // expire (unused)
	}
	_, _ = d.readString() // clientMsgNo (unused)
	messageID, err := d.readInt64()
	if err != nil {
		return
	}
	messageSeq, _ := d.readInt32()
	timestamp, _ := d.readInt32()
	readTopicIfPresent(d, setting)
	encrypted := d.readRemaining()

	// A truncated/short frame leaves channelID empty (decoder returns errShort +
	// zero value). Acking and forwarding such a message would route an
	// unaddressable turn, so drop it before the ack (L25). messageID already
	// guarded above.
	if cerr != nil || channelID == "" {
		return
	}

	idStr := strconv.FormatUint(messageID, 10)

	plain, derr := aesDecryptPayload(encrypted, s.aesKey, s.aesIV)
	if derr != nil {
		s.handleDecryptFailure(idStr, messageID, messageSeq, derr)
		return
	}
	// Success: clear failure count, ack (after successful decrypt+parse), forward.
	payload, perr := parsePayload(plain)
	if perr != nil {
		s.handleDecryptFailure(idStr, messageID, messageSeq, perr)
		return
	}
	delete(s.decryptFails, idStr)
	_ = s.writeRaw(encodeRecvack(messageID, messageSeq))
	// Inbound dedup lives in the connector (onInbound tier-1 + drainTurns
	// tier-2), NOT here — the read loop stays pure decrypt→ack→dispatch so a
	// contended SQLite write can never stall keepalive/DISCONNECT detection.
	s.dispatchRecvMessage(idStr, messageSeq, fromUID, channelID, channelType, timestamp, payload, setting)
}

func (s *socketConn) dispatchRecvMessage(
	idStr string,
	messageSeq uint32,
	fromUID string,
	channelID string,
	channelType byte,
	timestamp uint32,
	payload MessagePayload,
	setting byte,
) {
	if s.onMessage != nil {
		s.onMessage(BotMessage{
			MessageID:   idStr,
			MessageSeq:  messageSeq,
			FromUID:     fromUID,
			ChannelID:   channelID,
			ChannelType: ChannelType(channelType),
			Timestamp:   timestamp,
			Payload:     payload,
			StreamOn:    settingStreamOn(setting),
		})
	}
}

func readTopicIfPresent(d *decoder, setting byte) {
	if settingTopic(setting) {
		_, _ = d.readString() // topic (unused)
	}
}

// handleDecryptFailure implements the 3-strike poison-drop (socket.ts): below
// the cap, do NOT ack (server redelivers); at the cap, ack-and-drop.
func (s *socketConn) handleDecryptFailure(idStr string, messageID uint64, messageSeq uint32, cause error) {
	s.decryptFails[idStr]++
	if s.decryptFails[idStr] >= maxDecryptRetries {
		_ = s.writeRaw(encodeRecvack(messageID, messageSeq)) // drop poison msg
		delete(s.decryptFails, idStr)
		if s.onError != nil {
			s.onError(fmt.Errorf("dropping undecryptable message %s: %w", idStr, cause))
		}
		return
	}
	// Bound the map WITHOUT discarding this message's strike count. Resetting the
	// whole map (or evicting a high-strike entry) could zero an in-flight poison
	// message's count so it never reaches maxDecryptRetries — the server would
	// then redeliver it forever (livelock). Evict the LOWEST-strike other entry
	// (a strike-1 entry has the least progress toward a drop, so losing its count
	// costs the least), falling back to any other entry.
	for len(s.decryptFails) > maxDecryptFailKeys {
		victim, victimStrikes := "", 0
		for k, n := range s.decryptFails {
			if k == idStr {
				continue
			}
			if victim == "" || n < victimStrikes {
				victim, victimStrikes = k, n
			}
		}
		if victim == "" {
			break
		}
		delete(s.decryptFails, victim)
	}
	// else: no ack → redelivery
}

// parsePayload decodes the decrypted JSON into a MessagePayload, defaulting
// type to 0 when absent (socket.ts builds { type: type ?? 0,... }).
func parsePayload(b []byte) (MessagePayload, error) {
	var p MessagePayload
	if err := jsonUnmarshal(b, &p); err != nil {
		return MessagePayload{}, err
	}
	return p, nil
}
