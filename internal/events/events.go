// Package events defines the event model shared by the generator, replay, feature-updater and scorer.
//
// JSON encoding is hand-written so that every kind emits exactly the fields its JSON Schema allows,
// in a fixed order, which keeps generator output byte-for-byte deterministic.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Kind of event.
type Kind uint8

// Event kinds.
const (
	KindLogin Kind = iota + 1
	KindDevice
	KindTransaction
)

// Schema names (mirrors internal/stream).
const (
	SchemaLogin       = "fraud.event.login.v1"
	SchemaDevice      = "fraud.event.device.v1"
	SchemaTransaction = "fraud.event.transaction.v1"
)

// Device change values.
const (
	ChangeDeviceBound     = "device_bound"
	ChangePasswordChanged = "password_changed"
	ChangeEmailChanged    = "email_changed"
	ChangePhoneChanged    = "phone_changed"
)

// Channel values as they appear in events (proto enum names without the CHANNEL_ prefix).
const (
	ChannelCardPresent = "CARD_PRESENT"
	ChannelEcommerce   = "ECOMMERCE"
	ChannelP2P         = "P2P"
)

// Event is one login, device or transaction event. Fields not used by a kind are empty.
type Event struct {
	Kind      Kind
	EventID   string
	AccountID string
	TSMillis  int64 // event_ts in Unix milliseconds (UTC)
	DeviceID  string
	IPPrefix  string
	Country   string

	// login
	Success bool
	Method  string

	// device
	Change string

	// transaction (TransactionID == EventID)
	AmountMinor  int64
	Currency     string
	MerchantID   string
	MerchantName string
	MCC          string
	Channel      string
}

// TopicFor returns the Kafka topic for the event kind.
func (e *Event) TopicFor() string {
	switch e.Kind {
	case KindLogin:
		return "events.login"
	case KindDevice:
		return "events.device"
	default:
		return "events.transaction"
	}
}

// FormatTS renders Unix milliseconds as RFC 3339 UTC with exactly three fractional digits.
func FormatTS(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// ParseTS parses an RFC 3339 timestamp into Unix milliseconds.
func ParseTS(s string) (int64, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, err
	}
	return t.UnixMilli(), nil
}

// AppendJSON appends the schema-shaped JSON encoding of e to dst.
func (e *Event) AppendJSON(dst []byte) []byte {
	str := func(b []byte, key, v string) []byte {
		b = append(b, ',', '"')
		b = append(b, key...)
		b = append(b, '"', ':')
		return strconv.AppendQuote(b, v)
	}
	switch e.Kind {
	case KindLogin:
		dst = append(dst, `{"schema":"`+SchemaLogin+`"`...)
	case KindDevice:
		dst = append(dst, `{"schema":"`+SchemaDevice+`"`...)
	case KindTransaction:
		dst = append(dst, `{"schema":"`+SchemaTransaction+`"`...)
	}
	dst = str(dst, "event_id", e.EventID)
	if e.Kind == KindTransaction {
		dst = str(dst, "transaction_id", e.EventID)
	}
	dst = str(dst, "account_id", e.AccountID)
	dst = str(dst, "event_ts", FormatTS(e.TSMillis))
	dst = str(dst, "device_id", e.DeviceID)
	switch e.Kind {
	case KindLogin:
		dst = str(dst, "ip_prefix", e.IPPrefix)
		dst = str(dst, "country", e.Country)
		dst = append(dst, `,"success":`...)
		dst = strconv.AppendBool(dst, e.Success)
		dst = str(dst, "method", e.Method)
	case KindDevice:
		dst = str(dst, "change", e.Change)
	case KindTransaction:
		dst = append(dst, `,"amount_minor":`...)
		dst = strconv.AppendInt(dst, e.AmountMinor, 10)
		dst = str(dst, "currency", e.Currency)
		dst = str(dst, "merchant_id", e.MerchantID)
		if e.MerchantName != "" {
			dst = str(dst, "merchant_name", e.MerchantName)
		}
		dst = str(dst, "mcc", e.MCC)
		dst = str(dst, "channel", e.Channel)
		dst = str(dst, "ip_prefix", e.IPPrefix)
		dst = str(dst, "country", e.Country)
	}
	return append(dst, '}')
}

// MarshalJSON implements json.Marshaler.
func (e *Event) MarshalJSON() ([]byte, error) { return e.AppendJSON(nil), nil }

type wire struct {
	Schema        string `json:"schema"`
	EventID       string `json:"event_id"`
	TransactionID string `json:"transaction_id"`
	AccountID     string `json:"account_id"`
	EventTS       string `json:"event_ts"`
	DeviceID      string `json:"device_id"`
	IPPrefix      string `json:"ip_prefix"`
	Country       string `json:"country"`
	Success       bool   `json:"success"`
	Method        string `json:"method"`
	Change        string `json:"change"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
	MerchantID    string `json:"merchant_id"`
	MerchantName  string `json:"merchant_name"`
	MCC           string `json:"mcc"`
	Channel       string `json:"channel"`
}

// ErrUnknownSchema is returned for messages whose schema is not an account event.
var ErrUnknownSchema = errors.New("unknown event schema")

// UnmarshalJSON implements json.Unmarshaler. It does not validate; use internal/stream for that.
func (e *Event) UnmarshalJSON(b []byte) error {
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	ts, err := ParseTS(w.EventTS)
	if err != nil {
		return fmt.Errorf("event_ts: %w", err)
	}
	*e = Event{
		EventID: w.EventID, AccountID: w.AccountID, TSMillis: ts, DeviceID: w.DeviceID,
		IPPrefix: w.IPPrefix, Country: w.Country, Success: w.Success, Method: w.Method, Change: w.Change,
		AmountMinor: w.AmountMinor, Currency: w.Currency, MerchantID: w.MerchantID,
		MerchantName: w.MerchantName, MCC: w.MCC, Channel: w.Channel,
	}
	switch w.Schema {
	case SchemaLogin:
		e.Kind = KindLogin
	case SchemaDevice:
		e.Kind = KindDevice
	case SchemaTransaction:
		e.Kind = KindTransaction
		if e.EventID == "" {
			e.EventID = w.TransactionID
		}
	default:
		return fmt.Errorf("%w: %q", ErrUnknownSchema, w.Schema)
	}
	return nil
}
