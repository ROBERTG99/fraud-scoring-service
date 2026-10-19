package api

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"
)

// The Score allowlists and the Kafka schema accept the same values (spec §15; audit M6 L4).
func TestAllowlistsMatchTheTransactionSchema(t *testing.T) {
	raw, err := os.ReadFile("../../schemas/events/transaction.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	for field, allow := range map[string]map[string]bool{"currency": CurrencyAllowlist, "mcc": MCCAllowlist} {
		got := slices.Sorted(maps.Keys(allow))
		want := slices.Sorted(slices.Values(schema.Properties[field].Enum))
		if !slices.Equal(got, want) {
			t.Errorf("%s: Go allowlist %v, schema enum %v", field, got, want)
		}
	}
}
