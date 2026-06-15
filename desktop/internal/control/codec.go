package control

import "encoding/json"

// marshalBody encodes a command body to raw JSON. A nil body yields an empty
// object so the daemon's decoders (which expect a body) are satisfied.
func marshalBody(body any) (json.RawMessage, error) {
	if body == nil {
		return json.RawMessage("{}"), nil
	}
	if raw, ok := body.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(body)
}
