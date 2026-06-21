// Package octo implements the Octo IM connector: the WuKongIM binary protocol
// (WebSocket) plus the Octo REST API, ported wire-compatibly from
// cc-channel-octo's src/octo. It produces router.InboundMessage values for the
// gateway and delivers replies via REST sendMessage.
//
// File layout (start at connector.go for the lifecycle):
//   - connector.go   — the orchestrator: connection lifecycle, per-session turn
//     queues, drainTurns (the sole writer of c.targets), persona OBO, typing
//     heartbeat, tool-progress notices, OnReply assembly + send.
//   - types.go       — wire enums (ChannelType, MessageType) and JSON-decoded
//     payload shapes shared by REST + WebSocket.
//   - rest.go        — REST client: send, history backfill, mention resolution,
//     media auth, /register flow.
//   - socket.go      — binary WebSocket protocol: handshake (curve25519 DH +
//     MD5→AES-128-CBC key derivation), encode/decode, ping loop.
//   - mention_out.go — outbound @mention resolution against the channel roster.
//   - inbound.go     — non-text payload → marker rendering, media materialization.
//
// The package is one connector behind the router.InboundMessage shape; the
// gateway is IM-agnostic and never imports octo.
package octo
