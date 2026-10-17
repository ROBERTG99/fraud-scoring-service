// Command benchreport turns a ghz JSON result into an evalgate.report.v1 (`latency` suite, spec §11) and
// fails when the profile's budget is missed: p95 (and p99 on the laptop profile) over budget, a success rate
// under 99.9%, or an achieved rate under 95% of the target (then the run did not measure "at N RPS").
//
// The p95 budget is carried as the report's budget (`extensions.budget.max_p95_ms` against
// `latency_p95_ms`), which `evalgate check` enforces. The paired cases are one-second windows with their
// success rate (gated) and p95 in ms (reported), so `evalgate check --baseline` compares like with like.
//
//	benchreport -in ghz.json -out evals/reports/latency.json -profile ci
//	benchreport -check-only -report evals/reports/latency.json [-allow-missing]
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Profile is a load level with its budget (spec §3.2 NFR-1, NFR-2).
type Profile struct {
	RPS      float64
	MaxP95Ms float64
	MaxP99Ms float64 // 0: not budgeted
}

// Profiles: ci gates CI (NFR-2), laptop is the NFR-1 target on a developer machine.
var Profiles = map[string]Profile{
	"ci":     {RPS: 300, MaxP95Ms: 80},
	"laptop": {RPS: 2000, MaxP95Ms: 50, MaxP99Ms: 90},
}

const (
	minSuccessRate = 0.999
	minRateShare   = 0.95
	// A degraded decision (model or features unavailable) is fast by construction; latency measured on
	// mostly degraded answers is not the scorer's latency, so more than 5% degraded fails the run.
	maxDegradedRate = 0.05
)

type ghzDetail struct {
	Timestamp time.Time `json:"timestamp"`
	Latency   int64     `json:"latency"` // ns
	Status    string    `json:"status"`
	Error     string    `json:"error"`
}

type ghzReport struct {
	Date    time.Time   `json:"date"`
	Count   int         `json:"count"`
	Total   int64       `json:"total"` // ns
	RPS     float64     `json:"rps"`
	Details []ghzDetail `json:"details"`
}

// Metric is a report.v1 metric summary.
type Metric struct {
	Value     float64  `json:"value"`
	Threshold *float64 `json:"threshold"`
	Direction string   `json:"direction"`
	Passed    bool     `json:"passed"`
	CILow     *float64 `json:"ci_low"`
	CIHigh    *float64 `json:"ci_high"`
}

// Case is a report.v1 case: one one-second window of the run.
type Case struct {
	ID        string             `json:"id"`
	Split     string             `json:"split"`
	Scores    map[string]float64 `json:"scores"`
	Passed    bool               `json:"passed"`
	CostUSD   float64            `json:"cost_usd"`
	LatencyMs float64            `json:"latency_ms"`
	Error     *string            `json:"error"`
}

// Report is evalgate.report.v1.
type Report struct {
	Schema       string            `json:"schema"`
	Suite        string            `json:"suite"`
	RunID        string            `json:"run_id"`
	CreatedAt    string            `json:"created_at"`
	GitSHA       *string           `json:"git_sha"`
	Mode         string            `json:"mode"`
	Models       []string          `json:"models"`
	PromptHashes map[string]string `json:"prompt_hashes"`
	NCases       int               `json:"n_cases"`
	Metrics      map[string]Metric `json:"metrics"`
	CostTotal    float64           `json:"cost_usd_total"`
	CostPerCase  float64           `json:"cost_usd_per_case"`
	LatencyP50   float64           `json:"latency_p50_ms"`
	LatencyP95   float64           `json:"latency_p95_ms"`
	Cases        []Case            `json:"cases"`
	Extensions   map[string]any    `json:"extensions"`
}

type options struct {
	in, out, profile        string
	checkOnly, allowMissing bool
	report                  string
	// decisions and degraded count the decisions persisted during the measured window (0: not known).
	decisions, degraded int
}

// percentile is the nearest-rank percentile of sorted values.
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	k := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[max(0, min(k, len(sorted)-1))]
}

func ptr(v float64) *float64 { return &v }

func gitSHA() *string {
	if v := os.Getenv("GITHUB_SHA"); v != "" {
		return &v
	}
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return nil
	}
	s := strings.TrimSpace(string(out))
	return &s
}

func build(g ghzReport, name string, p Profile, decisions, degraded int) (Report, []string) {
	lat := make([]float64, 0, len(g.Details))
	ok := 0
	type window struct {
		ok, n int
		lat   []float64
	}
	var first time.Time
	if len(g.Details) > 0 {
		first = slices.MinFunc(g.Details, func(a, b ghzDetail) int { return a.Timestamp.Compare(b.Timestamp) }).Timestamp
	}
	windows := map[int]*window{}
	for _, d := range g.Details {
		ms := float64(d.Latency) / 1e6
		lat = append(lat, ms)
		good := d.Status == "OK" && d.Error == ""
		if good {
			ok++
		}
		k := int(d.Timestamp.Sub(first) / time.Second)
		w := windows[k]
		if w == nil {
			w = &window{}
			windows[k] = w
		}
		w.n++
		w.lat = append(w.lat, ms)
		if good {
			w.ok++
		}
	}
	slices.Sort(lat)
	n := len(lat)
	success := 0.0
	if n > 0 {
		success = float64(ok) / float64(n)
	}
	seconds := float64(g.Total) / 1e9
	achieved := 0.0
	if seconds > 0 {
		achieved = float64(n) / seconds
	}
	p50, p95, p99 := percentile(lat, 0.50), percentile(lat, 0.95), percentile(lat, 0.99)

	keys := make([]int, 0, len(windows))
	for k := range windows {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	cases := make([]Case, 0, len(keys))
	for _, k := range keys {
		w := windows[k]
		slices.Sort(w.lat)
		rate := float64(w.ok) / float64(w.n)
		cases = append(cases, Case{ID: fmt.Sprintf("window-%03d", k), Split: "test", Passed: rate >= minSuccessRate,
			Scores: map[string]float64{"success_rate": rate, "p95_ms": percentile(w.lat, 0.95)}, LatencyMs: percentile(w.lat, 0.5)})
	}

	var failures []string
	if n == 0 {
		failures = append(failures, "no requests in the ghz report")
	}
	if success < minSuccessRate {
		failures = append(failures, fmt.Sprintf("success_rate %.4f < %.3f", success, minSuccessRate))
	}
	if p95 > p.MaxP95Ms {
		failures = append(failures, fmt.Sprintf("p95 %.1f ms > %.0f ms", p95, p.MaxP95Ms))
	}
	if p.MaxP99Ms > 0 && p99 > p.MaxP99Ms {
		failures = append(failures, fmt.Sprintf("p99 %.1f ms > %.0f ms", p99, p.MaxP99Ms))
	}
	if achieved < minRateShare*p.RPS {
		failures = append(failures, fmt.Sprintf("achieved %.0f rps < %.0f%% of the %.0f rps target", achieved, minRateShare*100, p.RPS))
	}

	degradedRate := -1.0
	if decisions > 0 {
		degradedRate = float64(degraded) / float64(decisions)
		if degradedRate > maxDegradedRate {
			failures = append(failures, fmt.Sprintf("degraded_rate %.4f > %.2f (%d of %d decisions)", degradedRate, maxDegradedRate, degraded, decisions))
		}
	}

	budget := map[string]any{"max_p95_ms": p.MaxP95Ms}
	if p.MaxP99Ms > 0 {
		budget["max_p99_ms"] = p.MaxP99Ms
	}
	now := time.Now().UTC()
	errorRate := 1 - success
	health := "ok"
	if errorRate > 0.5 {
		health = "degraded"
	}
	p99Threshold := (*float64)(nil)
	if p.MaxP99Ms > 0 {
		p99Threshold = ptr(p.MaxP99Ms)
	}
	rep := Report{
		Schema: "evalgate.report.v1", Suite: "latency", RunID: now.Format("20060102T150405Z"), CreatedAt: now.Format(time.RFC3339Nano),
		GitSHA: gitSHA(), Mode: "offline", Models: []string{}, PromptHashes: map[string]string{}, NCases: len(cases),
		Metrics: map[string]Metric{
			"success_rate": {Value: success, Threshold: ptr(minSuccessRate), Direction: "higher", Passed: success >= minSuccessRate},
			"p95_ms":       {Value: p95, Threshold: ptr(p.MaxP95Ms), Direction: "lower", Passed: p95 <= p.MaxP95Ms},
			"p99_ms":       {Value: p99, Threshold: p99Threshold, Direction: "lower", Passed: p.MaxP99Ms == 0 || p99 <= p.MaxP99Ms},
		},
		LatencyP50: p50, LatencyP95: p95, Cases: cases,
		Extensions: map[string]any{
			"profile": name, "target_rps": p.RPS, "achieved_rps": achieved, "requests": n, "duration_s": seconds,
			"p99_ms": p99, "budget": budget, "split": "test",
			// p95/p99 are in ms: they gate through the budget, not as paired metrics (1 "pp" would be 0.01 ms).
			"gates":         map[string]bool{"success_rate": true, "p95_ms": false, "p99_ms": false},
			"target_errors": n - ok, "metric_errors": map[string]int{}, "run_health": health, "error_rate": errorRate,
			"failures": failures,
		},
	}
	if degradedRate >= 0 {
		rep.Metrics["degraded_rate"] = Metric{Value: degradedRate, Threshold: ptr(maxDegradedRate), Direction: "lower",
			Passed: degradedRate <= maxDegradedRate}
		rep.Extensions["gates"].(map[string]bool)["degraded_rate"] = false // counted per run, not per window
		rep.Extensions["decisions"], rep.Extensions["degraded"] = decisions, degraded
	}
	return rep, failures
}

func checkReport(rep Report) []string {
	var failures []string
	b, _ := rep.Extensions["budget"].(map[string]any)
	if v, ok := b["max_p95_ms"].(float64); ok && rep.LatencyP95 > v {
		failures = append(failures, fmt.Sprintf("p95 %.1f ms > %.0f ms", rep.LatencyP95, v))
	}
	p99, _ := rep.Extensions["p99_ms"].(float64)
	if v, ok := b["max_p99_ms"].(float64); ok && p99 > v {
		failures = append(failures, fmt.Sprintf("p99 %.1f ms > %.0f ms", p99, v))
	}
	for name, m := range rep.Metrics {
		if !m.Passed {
			failures = append(failures, name+" below its threshold")
		}
	}
	if f, ok := rep.Extensions["failures"].([]any); ok {
		for _, x := range f {
			if s, ok := x.(string); ok && strings.Contains(s, "rps") {
				failures = append(failures, s)
			}
		}
	}
	slices.Sort(failures)
	return slices.Compact(failures)
}

func run(o options) error {
	if o.checkOnly {
		raw, err := os.ReadFile(o.report)
		if errors.Is(err, fs.ErrNotExist) && o.allowMissing {
			fmt.Printf("benchreport: %s not found; run `make bench` to measure latency\n", o.report)
			return nil
		}
		if err != nil {
			return err
		}
		var rep Report
		if err := json.Unmarshal(raw, &rep); err != nil {
			return fmt.Errorf("%s: %w", o.report, err)
		}
		if f := checkReport(rep); len(f) > 0 {
			return fmt.Errorf("latency budget missed: %s", strings.Join(f, "; "))
		}
		fmt.Printf("benchreport: %s within budget (p95 %.1f ms)\n", o.report, rep.LatencyP95)
		return nil
	}
	p, ok := Profiles[o.profile]
	if !ok {
		return fmt.Errorf("unknown profile %q (ci or laptop)", o.profile)
	}
	raw, err := os.ReadFile(o.in)
	if err != nil {
		return err
	}
	var g ghzReport
	if err := json.Unmarshal(raw, &g); err != nil {
		return fmt.Errorf("%s: %w", o.in, err)
	}
	rep, failures := build(g, o.profile, p, o.decisions, o.degraded)
	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(o.out, append(out, '\n'), 0o600); err != nil {
		return err
	}
	degradedText := "unknown"
	if m, ok := rep.Metrics["degraded_rate"]; ok {
		degradedText = fmt.Sprintf("%.4f", m.Value)
	}
	fmt.Printf("latency (%s, %.0f rps target, %.0f achieved): p50 %.1f ms, p95 %.1f ms, p99 %.1f ms, success %.4f, degraded %s -> %s\n",
		o.profile, p.RPS, rep.Extensions["achieved_rps"], rep.LatencyP50, rep.LatencyP95, rep.Extensions["p99_ms"],
		rep.Metrics["success_rate"].Value, degradedText, o.out)
	if len(failures) > 0 {
		return fmt.Errorf("latency budget missed: %s", strings.Join(failures, "; "))
	}
	return nil
}

func main() {
	var o options
	flag.StringVar(&o.in, "in", "", "ghz JSON result")
	flag.StringVar(&o.out, "out", "evals/reports/latency.json", "report.v1 to write")
	flag.StringVar(&o.profile, "profile", "ci", "budget profile: ci (300 rps, p95 80 ms) or laptop (2000 rps, p95 50 ms, p99 90 ms)")
	flag.BoolVar(&o.checkOnly, "check-only", false, "only check an existing report against its budget")
	flag.StringVar(&o.report, "report", "evals/reports/latency.json", "report to check with -check-only")
	flag.BoolVar(&o.allowMissing, "allow-missing", false, "with -check-only, pass when the report does not exist")
	flag.IntVar(&o.decisions, "decisions", 0, "decisions persisted during the measured window (from Postgres)")
	flag.IntVar(&o.degraded, "degraded", 0, "of those, degraded decisions")
	flag.Parse()
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "benchreport:", err)
		os.Exit(1)
	}
}
