package events

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
)

func TestRoundTripAndSchemaValid(t *testing.T) {
	dev := strings.Repeat("ab", 32)
	cases := []Event{
		{Kind: KindLogin, EventID: "0192f000-0000-7000-8000-000000000001", AccountID: "acc_000001", TSMillis: 1788000000123, DeviceID: dev, IPPrefix: "203.0.113.0/24", Country: "US", Success: false, Method: "otp"},
		{Kind: KindDevice, EventID: "0192f000-0000-7000-8000-000000000002", AccountID: "acc_000001", TSMillis: 1788000000000, DeviceID: dev, Change: ChangeEmailChanged},
		{Kind: KindTransaction, EventID: "0192f000-0000-7000-8000-000000000003", AccountID: "acc_000001", TSMillis: 1788000001000, DeviceID: dev, IPPrefix: "198.51.100.0/24", Country: "GB", AmountMinor: 94000, Currency: "GBP", MerchantID: "m_0042", MerchantName: `Quote "and" backslash \ name`, MCC: "5732", Channel: ChannelEcommerce},
	}
	v := stream.MustValidator()
	for _, want := range cases {
		raw, err := json.Marshal(&want)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.ValidateTopic(want.TopicFor(), raw); err != nil {
			t.Fatalf("%s: %v\n%s", want.TopicFor(), err, raw)
		}
		var got Event
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
		}
	}
}

func TestFormatTSFixedMillis(t *testing.T) {
	if got := FormatTS(1788000000000); got != "2026-08-29T10:40:00.000Z" {
		t.Fatalf("FormatTS = %s", got)
	}
	ms, err := ParseTS("2026-08-29T12:40:00.5+02:00")
	if err != nil || ms != 1788000000500 {
		t.Fatalf("ParseTS = %d, %v", ms, err)
	}
}

func TestUnknownSchema(t *testing.T) {
	var e Event
	if err := json.Unmarshal([]byte(`{"schema":"fraud.decision.v1","event_ts":"2026-01-01T00:00:00Z"}`), &e); err == nil {
		t.Fatal("expected ErrUnknownSchema")
	}
}
