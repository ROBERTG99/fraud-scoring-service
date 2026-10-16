package stream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvalidEventRejected(t *testing.T) {
	v := MustValidator()
	raw := []byte(`{"schema":"fraud.event.login.v1","event_id":"0192f000-0000-7000-8000-000000000101","account_id":"acc_000123","event_ts":"2026-10-01T10:00:00Z","device_id":"` + strings.Repeat("a", 64) + `","ip_prefix":"203.0.113.0/24","country":"US","success":true}`)
	if err := v.ValidateTopic(TopicLogin, raw); err == nil {
		t.Fatal("login event without method must be rejected")
	} else if !strings.Contains(err.Error(), "method") {
		t.Fatalf("error should name the missing property: %v", err)
	}
	if err := v.ValidateTopic(TopicLogin, []byte(`{not json`)); err == nil {
		t.Fatal("malformed JSON must be rejected")
	}
	if err := v.ValidateTopic(TopicDevice, []byte(`{"schema":"fraud.event.login.v1"}`)); err == nil {
		t.Fatal("schema name must match the topic")
	}
	if err := v.ValidateTopic(TopicLogin, []byte(`{"schema":"fraud.event.unknown.v1"}`)); err == nil {
		t.Fatal("unknown schema must be rejected")
	}
}

// TestSharedFixtures checks the same fixtures as tests/unit/test_events.py::test_shared_fixtures_agree.
func TestSharedFixtures(t *testing.T) {
	dir := filepath.Join("..", "..", "tests", "fixtures", "events")
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest []struct {
		File  string `json:"file"`
		Kind  string `json:"kind"`
		Valid bool   `json:"valid"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 40 {
		t.Fatalf("manifest has %d fixtures, want 40", len(manifest))
	}
	v := MustValidator()
	for _, m := range manifest {
		raw, err := os.ReadFile(filepath.Join(dir, m.File))
		if err != nil {
			t.Fatal(err)
		}
		_, err = v.Validate(raw)
		if m.Valid && err != nil {
			t.Errorf("%s should be valid: %v", m.File, err)
		}
		if !m.Valid && err == nil {
			t.Errorf("%s should be invalid", m.File)
		}
	}
}
