package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ghzJSON writes a ghz JSON report: n requests spread evenly over the given seconds, latency(i) in ms.
func ghzJSON(t *testing.T, n, seconds int, latency func(i int) float64, failEvery int) string {
	t.Helper()
	start := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	details := make([]map[string]any, n)
	for i := range n {
		status, errText := "OK", ""
		if failEvery > 0 && i%failEvery == 0 {
			status, errText = "Unavailable", "rpc error: code = Unavailable"
		}
		ts := start.Add(time.Duration(i) * time.Duration(seconds) * time.Second / time.Duration(n))
		details[i] = map[string]any{"timestamp": ts.Format(time.RFC3339Nano), "latency": int64(latency(i) * 1e6), "status": status, "error": errText}
	}
	raw, err := json.Marshal(map[string]any{"date": start.Format(time.RFC3339), "count": n, "total": int64(seconds) * 1e9,
		"rps": float64(n) / float64(seconds), "details": details})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ghz.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBenchreportFailsOverBudget(t *testing.T) {
	in := ghzJSON(t, 3000, 10, func(i int) float64 { return 20 + float64(i%100) }, 0) // p95 ≈ 115 ms
	out := filepath.Join(t.TempDir(), "latency.json")
	err := run(options{in: in, out: out, profile: "ci"})
	if err == nil || !strings.Contains(err.Error(), "p95") {
		t.Fatalf("want a p95 budget failure, got %v", err)
	}
	var rep Report
	raw, _ := os.ReadFile(out)
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("the report is written even when the budget fails: %v", err)
	}
	if rep.Schema != "evalgate.report.v1" || rep.LatencyP95 < 110 || rep.Extensions["budget"].(map[string]any)["max_p95_ms"] != 80.0 {
		t.Fatalf("report %+v", rep)
	}
}

func TestBenchreportPassesWithinBudgetAndPairsWindows(t *testing.T) {
	in := ghzJSON(t, 3000, 10, func(i int) float64 { return 2 + float64(i%10) }, 0)
	out := filepath.Join(t.TempDir(), "latency.json")
	if err := run(options{in: in, out: out, profile: "ci"}); err != nil {
		t.Fatal(err)
	}
	var rep Report
	raw, _ := os.ReadFile(out)
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.NCases != 10 || len(rep.Cases) != 10 || rep.Cases[0].ID != "window-000" {
		t.Fatalf("one case per second: %d cases, first %+v", rep.NCases, rep.Cases[0])
	}
	if m := rep.Metrics["success_rate"]; m.Value != 1 || !m.Passed {
		t.Fatalf("success_rate %+v", m)
	}
	if rep.LatencyP95 > 12 || rep.LatencyP50 < 2 {
		t.Fatalf("p50 %v p95 %v", rep.LatencyP50, rep.LatencyP95)
	}
}

func TestBenchreportFailsOnErrorsAndOnAMissedRate(t *testing.T) {
	in := ghzJSON(t, 3000, 10, func(int) float64 { return 3 }, 50) // 2% errors
	if err := run(options{in: in, out: filepath.Join(t.TempDir(), "r.json"), profile: "ci"}); err == nil || !strings.Contains(err.Error(), "success_rate") {
		t.Fatalf("want a success_rate failure, got %v", err)
	}
	slow := ghzJSON(t, 1500, 10, func(int) float64 { return 3 }, 0) // 150 RPS achieved, 300 targeted
	if err := run(options{in: slow, out: filepath.Join(t.TempDir(), "r.json"), profile: "ci"}); err == nil || !strings.Contains(err.Error(), "rps") {
		t.Fatalf("want an achieved-rate failure, got %v", err)
	}
}

func TestCheckOnlyAllowsAMissingReport(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.json")
	if err := run(options{checkOnly: true, report: missing, allowMissing: true}); err != nil {
		t.Fatal(err)
	}
	if err := run(options{checkOnly: true, report: missing}); err == nil {
		t.Fatal("a missing report without -allow-missing must fail")
	}
	in := ghzJSON(t, 3000, 10, func(i int) float64 { return 20 + float64(i%100) }, 0)
	out := filepath.Join(t.TempDir(), "latency.json")
	_ = run(options{in: in, out: out, profile: "ci"})
	if err := run(options{checkOnly: true, report: out}); err == nil {
		t.Fatalf("check-only must fail an over-budget report %s", out)
	}
}

func TestBenchreportFailsWhenMostDecisionsDegrade(t *testing.T) {
	in := ghzJSON(t, 3000, 10, func(int) float64 { return 3 }, 0)
	out := filepath.Join(t.TempDir(), "latency.json")
	err := run(options{in: in, out: out, profile: "ci", decisionsKnown: true, decisions: 3000, degraded: 2910})
	if err == nil || !strings.Contains(err.Error(), "degraded_rate") {
		t.Fatalf("fast answers that are 97%% degraded must fail, got %v", err)
	}
	if err := run(options{in: in, out: out, profile: "ci", decisionsKnown: true, decisions: 3000, degraded: 45}); err != nil {
		t.Fatalf("1.5%% degraded is within the 5%% limit: %v", err)
	}
	var rep Report
	raw, _ := os.ReadFile(out)
	if err := json.Unmarshal(raw, &rep); err != nil || rep.Metrics["degraded_rate"].Value != 0.015 {
		t.Fatalf("degraded_rate %+v (%v)", rep.Metrics["degraded_rate"], err)
	}
}

func TestBenchreportFailsWhenDecisionsAreMissing(t *testing.T) {
	in := ghzJSON(t, 3000, 10, func(int) float64 { return 3 }, 0)
	for _, n := range []int{0, 2000} {
		err := run(options{in: in, out: filepath.Join(t.TempDir(), "r.json"), profile: "ci", decisionsKnown: true, decisions: n})
		if err == nil || !strings.Contains(err.Error(), "decisions persisted") {
			t.Fatalf("%d decisions for 3000 successful requests must fail, got %v", n, err)
		}
	}
}

func TestDirtyTreeIsStamped(t *testing.T) {
	if got := stampSHA("abc123", true); got != "abc123-dirty" {
		t.Fatalf("got %q", got)
	}
	if got := stampSHA("abc123", false); got != "abc123" {
		t.Fatalf("got %q", got)
	}
}
