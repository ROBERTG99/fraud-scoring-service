//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/model"
)

func seedCases(t *testing.T, pool *pgxpool.Pool, n int) []string {
	t.Helper()
	w, err := NewGroupWriter(pool, WriterConfig{SpoolDir: t.TempDir()}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var txIDs []string
	for i := 0; i < n; i++ {
		r := record(5000+i, fraudv1.Action_ACTION_REVIEW)
		r.Decision.Score = float32(i+1) / float32(n+1) // distinct priorities
		if i%2 == 0 {
			r.Decision.ReasonCodes = []string{"CARD_TESTING_BURST"}
		}
		if _, err := w.Commit(ctx, r); err != nil {
			t.Fatal(err)
		}
		txIDs = append(txIDs, r.Decision.TransactionId)
	}
	return txIDs
}

func caseIDs(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT case_id::text FROM review_cases ORDER BY priority DESC, case_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func TestLabelWritesAuditRow(t *testing.T) {
	pool := newPool(t, migratedDB(t))
	seedCases(t, pool, 3)
	s := NewCaseStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := caseIDs(t, pool)[0]
	c, err := s.LabelCase(ctx, LabelInput{CaseID: id, Label: "fraud", Note: "customer confirmed", IdempotencyKey: "key-0001", ActorSub: "analyst-1", Role: "analyst"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != fraudv1.CaseStatus_CASE_STATUS_LABELLED || c.Label.GetLabel() != fraudv1.Label_LABEL_FRAUD || c.Label.AnalystSub != "analyst-1" {
		t.Fatalf("case %+v", c)
	}
	if n := count(t, pool, "SELECT count(*) FROM analyst_labels"); n != 1 {
		t.Fatalf("labels %d", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM audit_log WHERE action = 'case.label' AND target = $1 AND actor_sub = 'analyst-1'", "case/"+id); n != 1 {
		t.Fatalf("audit rows %d", n)
	}
	// The case leaves the open queue.
	open, _, total, err := s.ListCases(ctx, ListQuery{Status: "open", PageSize: 10})
	if err != nil || len(open) != 2 || total != 2 {
		t.Fatalf("open %d total %d %v", len(open), total, err)
	}
	// Replay with the same key, and the same label with a new key: no new rows.
	for _, key := range []string{"key-0001", "key-0002"} {
		if _, err := s.LabelCase(ctx, LabelInput{CaseID: id, Label: "fraud", IdempotencyKey: key, ActorSub: "analyst-1", Role: "analyst"}); err != nil {
			t.Fatal(err)
		}
	}
	if count(t, pool, "SELECT count(*) FROM analyst_labels") != 1 || count(t, pool, "SELECT count(*) FROM audit_log") != 1 {
		t.Fatal("replays wrote new rows")
	}
	// A conflicting label is rejected; a key reused for another case is rejected; unknown cases are not found.
	if _, err := s.LabelCase(ctx, LabelInput{CaseID: id, Label: "legit", IdempotencyKey: "key-0003", ActorSub: "analyst-1", Role: "analyst"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting label: %v", err)
	}
	other := caseIDs(t, pool)[1]
	if _, err := s.LabelCase(ctx, LabelInput{CaseID: other, Label: "fraud", IdempotencyKey: "key-0001", ActorSub: "analyst-1", Role: "analyst"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused key: %v", err)
	}
	if _, err := s.LabelCase(ctx, LabelInput{CaseID: "0192f000-0000-7000-8000-00000000dead", Label: "fraud", IdempotencyKey: "key-x", ActorSub: "a", Role: "analyst"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown case: %v", err)
	}
	if _, err := s.LabelCase(ctx, LabelInput{CaseID: other, Label: "legit", IdempotencyKey: "key-0004", ActorSub: "analyst-2", Role: "analyst"}); err != nil {
		t.Fatal(err)
	}
	if n, err := VerifyAudit(ctx, pool); err != nil || n != 2 {
		t.Fatalf("audit verify %d %v", n, err)
	}
}

func TestAuditVerifyDetectsTampering(t *testing.T) {
	dsn := migratedDB(t)
	pool := newPool(t, dsn)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		tx, _ := pool.Begin(ctx)
		if _, err := AppendAudit(ctx, tx, "analyst-1", "analyst", "case.label", fmt.Sprintf("case/%d", i), map[string]any{"label": "fraud", "n": i}); err != nil {
			t.Fatal(err)
		}
		_ = tx.Commit(ctx)
	}
	if n, err := VerifyAudit(ctx, pool); err != nil || n != 3 {
		t.Fatalf("verify %d %v", n, err)
	}
	// Superuser-only tampering (the trigger blocks normal updates).
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_log DISABLE TRIGGER audit_log_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_log SET details = '{"label":"legit","n":1}' WHERE seq = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAudit(ctx, pool); err == nil {
		t.Fatal("tampered chain verified")
	}
}

func TestListGetTimelineShadowAndRules(t *testing.T) {
	pool := newPool(t, migratedDB(t))
	seedCases(t, pool, 5)
	s := NewCaseStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Pagination, priority order and filters.
	page1, next, total, err := s.ListCases(ctx, ListQuery{Status: "open", PageSize: 2})
	if err != nil || len(page1) != 2 || next == "" || total != 5 || page1[0].Priority < page1[1].Priority {
		t.Fatalf("page1 %v %q %d %v", len(page1), next, total, err)
	}
	page2, next2, _, err := s.ListCases(ctx, ListQuery{Status: "open", PageSize: 2, PageToken: next})
	if err != nil || len(page2) != 2 || next2 == "" || page2[0].CaseId == page1[1].CaseId || page2[0].Priority > page1[1].Priority {
		t.Fatalf("page2 %v %v", page2, err)
	}
	page3, next3, _, _ := s.ListCases(ctx, ListQuery{Status: "open", PageSize: 2, PageToken: next2})
	if len(page3) != 1 || next3 != "" {
		t.Fatalf("page3 %d %q", len(page3), next3)
	}
	if _, _, _, err := s.ListCases(ctx, ListQuery{Status: "open", PageSize: 2, PageToken: "garbage"}); !errors.Is(err, ErrBadPageToken) {
		t.Fatalf("bad token: %v", err)
	}
	burst, _, _, _ := s.ListCases(ctx, ListQuery{Status: "open", PageSize: 10, ReasonCode: "CARD_TESTING_BURST"})
	pending, _, _, _ := s.ListCases(ctx, ListQuery{Status: "open", PageSize: 10, SummaryStatus: "pending"})
	if len(burst) != 3 || len(pending) != 5 || page1[0].SummaryStatus != "pending" || page1[0].AmountMinor != 94000 || page1[0].Currency != "USD" {
		t.Fatalf("filters burst=%d pending=%d %+v", len(burst), len(pending), page1[0])
	}
	// GetCase with facts rebuilt from the decision, then with a stored summary.
	c, err := s.GetCase(ctx, page1[0].CaseId)
	if err != nil {
		t.Fatal(err)
	}
	facts := map[string]string{}
	for _, f := range c.Facts {
		facts[f.Key] = f.Value
	}
	if facts["feature.amount_minor"] != "94000" || facts["decision.action"] != "REVIEW" || facts["event.transaction.0.mcc"] != "5732" {
		t.Fatalf("facts %v", facts)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO case_summaries (case_id, status, summary, pattern, faithfulness, model, prompt_version, cost_usd)
		VALUES ($1, 'ok', $2, 'account_takeover', 1, 'claude-haiku-5-5', 'case_summary.v1', 0.0002)`, c.CaseId,
		`{"pattern":"account_takeover","evidence":[{"fact_key":"feature.amount_minor","statement":"Amount 94000.","verified":true}],"recommended_checks":["Call"],"confidence":0.8,"model":"claude-haiku-5-5","prompt_version":"case_summary.v1","faithfulness":1,"status":"ok","facts":{"feature.amount_minor":"94000","decision.score":"0.83"}}`); err != nil {
		t.Fatal(err)
	}
	c, _ = s.GetCase(ctx, c.CaseId)
	if c.Summary == nil || c.Summary.Pattern != "account_takeover" || !c.Summary.Evidence[0].Verified || c.SummaryStatus != "ok" || len(c.Facts) != 2 {
		t.Fatalf("summary %+v facts %v", c.Summary, c.Facts)
	}
	if _, err := s.GetCase(ctx, "0192f000-0000-7000-8000-00000000beef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown case: %v", err)
	}
	// Timeline: account events + decisions + labels, newest first.
	if err := InsertAccountEvents(ctx, pool, []events.Event{{Kind: events.KindLogin, EventID: "ev-1", AccountID: "acc_00000042", TSMillis: time.Date(2026, 9, 20, 10, 2, 0, 0, time.UTC).UnixMilli(), DeviceID: "d", Success: true, Method: "password"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LabelCase(ctx, LabelInput{CaseID: c.CaseId, Label: "fraud", IdempotencyKey: "tl-key-1", ActorSub: "analyst-1", Role: "analyst"}); err != nil {
		t.Fatal(err)
	}
	items, err := s.Timeline(ctx, "acc_00000042", 100)
	if err != nil || len(items) != 7 || items[0].Kind != "label" {
		t.Fatalf("timeline %d %v first=%v", len(items), err, items)
	}
	kinds := map[string]int{}
	for _, it := range items {
		kinds[it.Kind]++
	}
	if kinds["decision"] != 5 || kinds["event.login"] != 1 || kinds["label"] != 1 {
		t.Fatalf("kinds %v", kinds)
	}
	// Shadow summary.
	shadow := NewShadowStore(pool)
	for i, cs := range page1 {
		_ = shadow.InsertComparison(ctx, compare(cs.Decision.DecisionId, i == 0))
	}
	sum, err := s.ShadowSummary(ctx, 24, 50)
	if err != nil || sum.Total != 2 || sum.AgreementRate != 0.5 || len(sum.Disagreements) != 1 || len(sum.BandMatrix) != 2 || sum.ShadowVersion != "fraud-gbdt/2" {
		t.Fatalf("shadow %+v %v", sum, err)
	}
	// Rule sets and samples.
	if err := s.SaveRuleSet(ctx, "2026.10.1", "abc", []byte(`{"version":"2026.10.1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRuleSet(ctx, "2026.10.2", "def", []byte(`{"version":"2026.10.2"}`)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, "SELECT count(*) FROM rule_sets WHERE active"); n != 1 {
		t.Fatalf("active rule sets %d", n)
	}
	samples, err := s.RecentSamples(ctx, 3)
	if err != nil || len(samples) != 3 || samples[0].Tx.MCC != "5732" || samples[0].Features["amount_minor"] != 94000 {
		t.Fatalf("samples %+v %v", samples, err)
	}
}

func compare(decisionID string, agree bool) model.Comparison {
	c := model.Comparison{DecisionID: decisionID, PrimaryVersion: "fraud-gbdt/1", ShadowVersion: "fraud-gbdt/2", PrimaryScore: 0.4, ShadowScore: 0.45,
		PrimaryBand: "REVIEW", ShadowBand: "REVIEW", Agree: true, CreatedAt: time.Now()}
	if !agree {
		c.ShadowScore, c.ShadowBand, c.Agree = 0.9, "DECLINE", false
	}
	return c
}
