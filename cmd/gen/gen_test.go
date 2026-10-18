package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"math"
	"testing"

	"github.com/ROBERTG99/fraud-scoring-service/internal/api"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
)

func generateDemo(t *testing.T) (*Dataset, []byte, []byte) {
	t.Helper()
	cfg, err := ConfigFor("demo", 7)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var ev, lb bytes.Buffer
	if err := ds.WriteEvents(&ev); err != nil {
		t.Fatal(err)
	}
	if err := ds.WriteLabels(&lb); err != nil {
		t.Fatal(err)
	}
	return ds, ev.Bytes(), lb.Bytes()
}

func TestDeterministicSHA(t *testing.T) {
	_, ev1, lb1 := generateDemo(t)
	_, ev2, lb2 := generateDemo(t)
	if sha256.Sum256(ev1) != sha256.Sum256(ev2) {
		t.Fatal("events.jsonl differs between two runs with seed 7")
	}
	if sha256.Sum256(lb1) != sha256.Sum256(lb2) {
		t.Fatal("labels.csv differs between two runs with seed 7")
	}
	cfg, _ := ConfigFor("demo", 8)
	other, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var ev3 bytes.Buffer
	if err := other.WriteEvents(&ev3); err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(ev3.Bytes()) == sha256.Sum256(ev1) {
		t.Fatal("a different seed must produce different data")
	}
}

func TestExactEventCountAndOrder(t *testing.T) {
	ds, ev, _ := generateDemo(t)
	if len(ds.Events) != 200_000 {
		t.Fatalf("got %d events, want 200000", len(ds.Events))
	}
	if n := bytes.Count(ev, []byte("\n")); n != 200_000 {
		t.Fatalf("events.jsonl has %d lines", n)
	}
	lastTS := map[string]int64{}
	for i, e := range ds.Events {
		if i > 0 && e.TSMillis < ds.Events[i-1].TSMillis {
			t.Fatalf("events not sorted by time at %d", i)
		}
		if prev, ok := lastTS[e.AccountID]; ok && e.TSMillis <= prev {
			t.Fatalf("account %s has non-increasing timestamps (%d <= %d)", e.AccountID, e.TSMillis, prev)
		}
		lastTS[e.AccountID] = e.TSMillis
	}
}

func TestFraudRateWithinTolerance(t *testing.T) {
	ds, _, _ := generateDemo(t)
	var tx, fraud int
	byPattern := map[string]int{}
	for _, l := range ds.Labels {
		tx++
		if l.Fraud {
			fraud++
			byPattern[l.Pattern]++
		}
	}
	rate := float64(fraud) / float64(tx)
	if math.Abs(rate-0.008) > 0.0005 {
		t.Fatalf("fraud rate %.5f outside 0.008 ± 0.0005 (%d / %d)", rate, fraud, tx)
	}
	for _, p := range []string{PatternATO, PatternCardTesting, PatternMule, PatternFriendly} {
		share := float64(byPattern[p]) / float64(fraud)
		if share < 0.04 {
			t.Errorf("pattern %s share %.3f too small", p, share)
		}
	}
}

func TestEventsValidateAgainstSchemas(t *testing.T) {
	ds, _, _ := generateDemo(t)
	v := stream.MustValidator()
	seen := map[events.Kind]int{}
	for i := 0; i < len(ds.Events); i += 97 {
		e := ds.Events[i]
		raw, _ := json.Marshal(&e)
		if err := v.ValidateTopic(e.TopicFor(), raw); err != nil {
			t.Fatalf("event %d invalid: %v\n%s", i, err, raw)
		}
		seen[e.Kind]++
	}
	if seen[events.KindLogin] == 0 || seen[events.KindDevice] == 0 || seen[events.KindTransaction] == 0 {
		t.Fatalf("missing kinds in sample: %v", seen)
	}
}

func TestTemporalSplitByDay(t *testing.T) {
	ds, _, _ := generateDemo(t)
	counts := map[string]int{}
	bounds := map[string][2]int64{}
	for _, l := range ds.Labels {
		counts[l.Split]++
		b, ok := bounds[l.Split]
		if !ok {
			b = [2]int64{l.TSMillis, l.TSMillis}
		}
		b[0] = min(b[0], l.TSMillis)
		b[1] = max(b[1], l.TSMillis)
		bounds[l.Split] = b
	}
	if bounds["train"][1] >= bounds["valid"][0] || bounds["valid"][1] >= bounds["test"][0] {
		t.Fatalf("splits overlap in time: %v", bounds)
	}
	total := counts["train"] + counts["valid"] + counts["test"]
	trainShare := float64(counts["train"]) / float64(total)
	if trainShare < 0.66 || trainShare > 0.74 {
		t.Fatalf("train share %.3f, want about 0.70 (%v)", trainShare, counts)
	}
	if counts["warmup"] == 0 {
		t.Fatal("expected warm-up transactions excluded from splits")
	}
}

// Every MCC the generator emits passes Score's allowlist (D-051).
func TestCatalogMCCsAreAllowlisted(t *testing.T) {
	for _, c := range categories {
		if !api.MCCAllowlist[c.MCC] {
			t.Errorf("catalog MCC %s (%s) is not in api.MCCAllowlist", c.MCC, c.Name)
		}
	}
	if len(api.MCCAllowlist) != len(categories) {
		t.Errorf("allowlist has %d MCCs, the catalog %d", len(api.MCCAllowlist), len(categories))
	}
}
