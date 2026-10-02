package charger

import (
	"encoding/json"
	"errors"
)

var (
	errOffline = errors.New("charger offline")
	errTimeout = errors.New("call timed out")
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
