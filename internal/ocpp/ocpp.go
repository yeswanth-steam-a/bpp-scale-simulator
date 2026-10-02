// Package ocpp holds the minimal OCPP 1.6J framing and payloads the simulated chargers speak.
package ocpp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

const (
	Call       = 2
	CallResult = 3
	CallError  = 4

	Subprotocol = "ocpp1.6"
	// Matches the CSMS pattern yyyy-MM-dd'T'HH:mm:ss[.SSS]XXX
	TimeFormat = "2006-01-02T15:04:05.000Z07:00"
)

func Now() string { return time.Now().UTC().Format(TimeFormat) }

// Frame is a decoded [type, id, ...] message.
type Frame struct {
	Type    int
	ID      string
	Action  string          // CALL only
	Payload json.RawMessage // CALL payload / CALLRESULT payload
	ErrCode string          // CALLERROR only
	ErrDesc string
}

func EncodeCall(id, action string, payload any) ([]byte, error) {
	p, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf(`[2,%q,%q,%s]`, id, action, p)), nil
}

func EncodeResult(id string, payload any) ([]byte, error) {
	p, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf(`[3,%q,%s]`, id, p)), nil
}

func Decode(b []byte) (*Frame, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) < 3 {
		return nil, fmt.Errorf("bad frame: %.80s", b)
	}
	var f Frame
	if err := json.Unmarshal(raw[0], &f.Type); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw[1], &f.ID); err != nil {
		return nil, err
	}
	switch f.Type {
	case Call:
		if len(raw) < 4 {
			return nil, fmt.Errorf("short CALL")
		}
		if err := json.Unmarshal(raw[2], &f.Action); err != nil {
			return nil, err
		}
		f.Payload = raw[3]
	case CallResult:
		f.Payload = raw[2]
	case CallError:
		_ = json.Unmarshal(raw[2], &f.ErrCode)
		if len(raw) > 3 {
			_ = json.Unmarshal(raw[3], &f.ErrDesc)
		}
	default:
		return nil, fmt.Errorf("unknown message type %d", f.Type)
	}
	return &f, nil
}

// ---- payloads (field names per OCPP 1.6J) ----

type BootNotificationReq struct {
	ChargePointVendor       string `json:"chargePointVendor"`
	ChargePointModel        string `json:"chargePointModel"`
	ChargePointSerialNumber string `json:"chargePointSerialNumber,omitempty"`
	FirmwareVersion         string `json:"firmwareVersion,omitempty"`
}

type BootNotificationRes struct {
	Status      string `json:"status"`
	Interval    int    `json:"interval"`
	CurrentTime string `json:"currentTime"`
}

type StatusNotificationReq struct {
	ConnectorID     int    `json:"connectorId"`
	ErrorCode       string `json:"errorCode"`
	Status          string `json:"status"`
	Info            string `json:"info,omitempty"`
	Timestamp       string `json:"timestamp,omitempty"`
	VendorErrorCode string `json:"vendorErrorCode,omitempty"`
}

type AuthorizeReq struct {
	IDTag string `json:"idTag"`
}

type IDTagInfo struct {
	Status string `json:"status"`
}

type AuthorizeRes struct {
	IDTagInfo IDTagInfo `json:"idTagInfo"`
}

type StartTransactionReq struct {
	ConnectorID int    `json:"connectorId"`
	IDTag       string `json:"idTag"`
	MeterStart  int    `json:"meterStart"`
	Timestamp   string `json:"timestamp"`
}

type StartTransactionRes struct {
	IDTagInfo     IDTagInfo `json:"idTagInfo"`
	TransactionID int       `json:"transactionId"`
}

type StopTransactionReq struct {
	IDTag         string `json:"idTag,omitempty"`
	MeterStop     int    `json:"meterStop"`
	Timestamp     string `json:"timestamp"`
	TransactionID int    `json:"transactionId"`
	Reason        string `json:"reason,omitempty"`
}

type SampledValue struct {
	Value     string `json:"value"`
	Context   string `json:"context,omitempty"`
	Format    string `json:"format,omitempty"`
	Measurand string `json:"measurand,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Location  string `json:"location,omitempty"`
	Unit      string `json:"unit,omitempty"`
}

type MeterValue struct {
	Timestamp    string         `json:"timestamp"`
	SampledValue []SampledValue `json:"sampledValue"`
}

type MeterValuesReq struct {
	ConnectorID   int          `json:"connectorId"`
	TransactionID *int         `json:"transactionId,omitempty"`
	MeterValue    []MeterValue `json:"meterValue"`
}

func Num(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }
