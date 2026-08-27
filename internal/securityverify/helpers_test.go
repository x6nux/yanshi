package securityverify

import "encoding/json"

// jsonObj marshals a small string map into the JSON a tool call carries.
func jsonObj(m map[string]string) (string, error) {
	b, err := json.Marshal(m)
	return string(b), err
}
