// Package contract holds cross-language contract tests.
package contract

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
)

// TestDecisionFieldsStable pins the §4.4 wire contract: renumbering or retyping a field breaks clients.
func TestDecisionFieldsStable(t *testing.T) {
	want := map[string]struct {
		num  protoreflect.FieldNumber
		kind protoreflect.Kind
		list bool
	}{
		"decision_id":    {1, protoreflect.StringKind, false},
		"transaction_id": {2, protoreflect.StringKind, false},
		"action":         {3, protoreflect.EnumKind, false},
		"score":          {4, protoreflect.FloatKind, false},
		"reason_codes":   {5, protoreflect.StringKind, true},
		"model_version":  {6, protoreflect.StringKind, false},
		"rules_version":  {7, protoreflect.StringKind, false},
		"latency_ms":     {8, protoreflect.FloatKind, false},
		"degraded":       {9, protoreflect.BoolKind, false},
		"features":       {10, protoreflect.MessageKind, false},
		"decided_at":     {11, protoreflect.MessageKind, false},
	}
	fields := (&fraudv1.Decision{}).ProtoReflect().Descriptor().Fields()
	if fields.Len() != len(want) {
		t.Fatalf("Decision has %d fields, contract has %d", fields.Len(), len(want))
	}
	for name, w := range want {
		f := fields.ByName(protoreflect.Name(name))
		if f == nil {
			t.Fatalf("Decision.%s missing", name)
		}
		if f.Number() != w.num || f.Kind() != w.kind || f.IsList() != w.list {
			t.Errorf("Decision.%s = #%d %v list=%v, want #%d %v list=%v", name, f.Number(), f.Kind(), f.IsList(), w.num, w.kind, w.list)
		}
	}
	if !fields.ByName("features").IsMap() {
		t.Error("Decision.features must be map<string,double>")
	}
}

func TestTransactionFieldsStable(t *testing.T) {
	want := []string{"transaction_id", "account_id", "amount_minor", "currency", "merchant_id", "mcc", "channel", "device_id", "ip_prefix", "country", "event_ts", "merchant_name"}
	fields := (&fraudv1.Transaction{}).ProtoReflect().Descriptor().Fields()
	for i, name := range want {
		f := fields.ByNumber(protoreflect.FieldNumber(i + 1))
		if f == nil || string(f.Name()) != name {
			t.Fatalf("Transaction field #%d should be %s", i+1, name)
		}
	}
	if fields.ByName("amount_minor").Kind() != protoreflect.Int64Kind {
		t.Fatal("amount_minor must be int64")
	}
}

func TestActionEnumValuesStable(t *testing.T) {
	cases := map[fraudv1.Action]string{0: "ACTION_UNSPECIFIED", 1: "ACTION_APPROVE", 2: "ACTION_REVIEW", 3: "ACTION_DECLINE"}
	for v, name := range cases {
		if v.String() != name {
			t.Errorf("Action(%d) = %s, want %s", v, v.String(), name)
		}
	}
}
