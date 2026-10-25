package ssh

import (
	"encoding/json"
	"strings"
)

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(strings.TrimSpace(s)), v) }
