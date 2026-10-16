package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
)

// Errors returned by CaseStore.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// CaseStore serves the analyst console (CaseService).
type CaseStore struct{ pool *pgxpool.Pool }

// NewCaseStore returns the store.
func NewCaseStore(pool *pgxpool.Pool) *CaseStore { return &CaseStore{pool: pool} }

// ListQuery filters ListCases.
type ListQuery struct {
	Status        string // open | labelled
	ReasonCode    string
	SummaryStatus string // pending | ok | fallback | unfaithful
	PageSize      int
	PageToken     string
}

const caseColumns = `rc.case_id::text, rc.status, rc.priority, rc.created_at,
	d.decision_id::text, d.transaction_id, d.account_id, d.action, d.score, d.reason_codes, d.model_version, d.rules_version, d.degraded,
	d.latency_ms, d.decided_at, d.features, d.tx,
	cs.status, cs.summary, cs.created_at,
	al.label, al.note, al.analyst_sub, al.created_at`

const caseFrom = ` FROM review_cases rc JOIN decisions d ON d.decision_id = rc.decision_id
	LEFT JOIN case_summaries cs ON cs.case_id = rc.case_id
	LEFT JOIN analyst_labels al ON al.case_id = rc.case_id`

type caseRow struct {
	c        *fraudv1.Case
	features map[string]float64
	tx       TxAttrs
	summary  []byte
}

func scanCase(row pgx.Row) (*caseRow, error) {
	r := caseRow{c: &fraudv1.Case{}}
	var (
		status, action           string
		score                    *float32
		created, decided         time.Time
		featuresRaw, txRaw       []byte
		sumStatus                *string
		sumCreated, labelCreated *time.Time
		label, note, analyst     *string
		d                        fraudv1.Decision
	)
	if err := row.Scan(&r.c.CaseId, &status, &r.c.Priority, &created, &d.DecisionId, &d.TransactionId, &r.c.AccountId, &action, &score,
		&d.ReasonCodes, &d.ModelVersion, &d.RulesVersion, &d.Degraded, &d.LatencyMs, &decided, &featuresRaw, &txRaw,
		&sumStatus, &r.summary, &sumCreated, &label, &note, &analyst, &labelCreated); err != nil {
		return nil, err
	}
	d.Action = fraudv1.Action(fraudv1.Action_value["ACTION_"+action])
	d.Score = -1
	if score != nil {
		d.Score = *score
	}
	d.DecidedAt = timestamppb.New(decided)
	r.c.Decision = &d
	r.c.CreatedAt = timestamppb.New(created)
	r.c.Status = fraudv1.CaseStatus_CASE_STATUS_OPEN
	if status == "labelled" {
		r.c.Status = fraudv1.CaseStatus_CASE_STATUS_LABELLED
	}
	_ = json.Unmarshal(featuresRaw, &r.features)
	_ = json.Unmarshal(txRaw, &r.tx)
	r.c.AmountMinor = r.tx.AmountMinor
	r.c.Currency = r.tx.Currency
	r.c.SummaryStatus = "pending"
	if sumStatus != nil {
		r.c.SummaryStatus = *sumStatus
	}
	if label != nil {
		l := &fraudv1.CaseLabel{Label: fraudv1.Label_LABEL_LEGIT, AnalystSub: deref(analyst)}
		if *label == "fraud" {
			l.Label = fraudv1.Label_LABEL_FRAUD
		}
		l.Note = deref(note)
		if labelCreated != nil {
			l.CreatedAt = timestamppb.New(*labelCreated)
		}
		r.c.Label = l
	}
	if sumCreated != nil && len(r.summary) > 0 {
		r.c.Summary = decodeSummary(r.summary, *sumCreated)
	}
	return &r, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

type storedSummary struct {
	Pattern  string `json:"pattern"`
	Evidence []struct {
		FactKey   string `json:"fact_key"`
		Statement string `json:"statement"`
		Verified  bool   `json:"verified"`
	} `json:"evidence"`
	RecommendedChecks []string          `json:"recommended_checks"`
	Confidence        float64           `json:"confidence"`
	Model             string            `json:"model"`
	PromptVersion     string            `json:"prompt_version"`
	Faithfulness      float64           `json:"faithfulness"`
	Status            string            `json:"status"`
	Facts             map[string]string `json:"facts"`
}

func decodeSummary(raw []byte, created time.Time) *fraudv1.CaseSummary {
	var s storedSummary
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	out := &fraudv1.CaseSummary{Status: s.Status, Pattern: s.Pattern, RecommendedChecks: s.RecommendedChecks, Confidence: s.Confidence,
		Model: s.Model, PromptVersion: s.PromptVersion, Faithfulness: s.Faithfulness, CreatedAt: timestamppb.New(created)}
	for _, e := range s.Evidence {
		out.Evidence = append(out.Evidence, &fraudv1.Evidence{FactKey: e.FactKey, Statement: e.Statement, Verified: e.Verified})
	}
	return out
}

// FormatFactNumber renders numbers like the summarizer (integers without decimals, up to 4 decimals).
func FormatFactNumber(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	s := strconv.FormatFloat(v, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// facts returns the facts the summary cited (stored by the summarizer) or, without a summary, the
// decision and feature facts rebuilt here with the same keys.
func (r *caseRow) facts() []*fraudv1.Fact {
	m := map[string]string{}
	var s storedSummary
	if len(r.summary) > 0 && json.Unmarshal(r.summary, &s) == nil && len(s.Facts) > 0 {
		m = s.Facts
	} else {
		d := r.c.Decision
		m["decision.action"] = ActionName(d.Action)
		m["decision.score"] = FormatFactNumber(float64(d.Score))
		m["decision.reason_codes"] = strings.Join(d.ReasonCodes, ",")
		m["decision.model_version"] = d.ModelVersion
		m["decision.rules_version"] = d.RulesVersion
		m["decision.degraded"] = strconv.FormatBool(d.Degraded)
		for k, v := range r.features {
			m["feature."+k] = FormatFactNumber(v)
		}
		m["event.transaction.0.mcc"] = r.tx.MCC
		m["event.transaction.0.channel"] = r.tx.Channel
		m["event.transaction.0.currency"] = r.tx.Currency
		m["event.transaction.0.country"] = r.tx.Country
		if r.tx.MerchantName != "" {
			m["event.transaction.0.merchant_name"] = r.tx.MerchantName
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*fraudv1.Fact, 0, len(keys))
	for _, k := range keys {
		out = append(out, &fraudv1.Fact{Key: k, Value: m[k]})
	}
	return out
}

func encodeToken(priority float32, caseID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatFloat(float64(priority), 'g', -1, 32) + "|" + caseID))
}

func decodeToken(tok string) (float32, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return 0, "", err
	}
	p, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return 0, "", errors.New("bad token")
	}
	f, err := strconv.ParseFloat(p, 32)
	if err != nil {
		return 0, "", err
	}
	if _, err := uuid.Parse(id); err != nil {
		return 0, "", err
	}
	return float32(f), id, nil
}

// ErrBadPageToken is returned for a malformed page_token.
var ErrBadPageToken = errors.New("invalid page_token")

// ListCases returns one page ordered by priority (highest first), the next page token and the open count.
func (s *CaseStore) ListCases(ctx context.Context, q ListQuery) ([]*fraudv1.Case, string, int32, error) {
	args := []any{q.Status, q.ReasonCode, q.SummaryStatus, q.PageSize + 1}
	where := ` WHERE rc.status = $1 AND ($2 = '' OR $2 = ANY(d.reason_codes))
		AND ($3 = '' OR ($3 = 'pending' AND cs.case_id IS NULL) OR cs.status = $3)`
	if q.PageToken != "" {
		p, id, err := decodeToken(q.PageToken)
		if err != nil {
			return nil, "", 0, ErrBadPageToken
		}
		args = append(args, p, id)
		where += ` AND (rc.priority < $5::real OR (rc.priority = $5::real AND rc.case_id > $6::uuid))`
	}
	rows, err := s.pool.Query(ctx, `SELECT `+caseColumns+caseFrom+where+` ORDER BY rc.priority DESC, rc.case_id LIMIT $4`, args...)
	if err != nil {
		return nil, "", 0, err
	}
	defer rows.Close()
	var out []*fraudv1.Case
	for rows.Next() {
		r, err := scanCase(rows)
		if err != nil {
			return nil, "", 0, err
		}
		out = append(out, r.c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", 0, err
	}
	next := ""
	if len(out) > q.PageSize {
		out = out[:q.PageSize]
		last := out[len(out)-1]
		next = encodeToken(last.Priority, last.CaseId)
	}
	var open int32
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM review_cases WHERE status = 'open'`).Scan(&open); err != nil {
		return nil, "", 0, err
	}
	return out, next, open, nil
}

// GetCase returns a case with facts and summary.
func (s *CaseStore) GetCase(ctx context.Context, caseID string) (*fraudv1.Case, error) {
	return s.getCase(ctx, s.pool, caseID)
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *CaseStore) getCase(ctx context.Context, q rowQuerier, caseID string) (*fraudv1.Case, error) {
	r, err := scanCase(q.QueryRow(ctx, `SELECT `+caseColumns+caseFrom+` WHERE rc.case_id = $1::uuid`, caseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.c.Facts = r.facts()
	return r.c, nil
}

// LabelInput is one analyst label.
type LabelInput struct {
	CaseID         string
	Label          string // fraud | legit
	Note           string
	IdempotencyKey string
	ActorSub       string
	Role           string
}

// LabelCase writes one analyst_labels row, marks the case labelled and appends one audit_log row, in one
// transaction. Replays with the same idempotency key, or the same label again, write nothing new; a
// different label on an already-labelled case is ErrConflict.
func (s *CaseStore) LabelCase(ctx context.Context, in LabelInput) (*fraudv1.Case, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM review_cases WHERE case_id = $1::uuid FOR UPDATE`, in.CaseID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var existingCase, existingLabel, existingKey string
	err = tx.QueryRow(ctx, `SELECT case_id::text, label, idempotency_key FROM analyst_labels WHERE case_id = $1::uuid OR idempotency_key = $2
		ORDER BY (idempotency_key = $2) DESC LIMIT 1`, in.CaseID, in.IdempotencyKey).Scan(&existingCase, &existingLabel, &existingKey)
	switch {
	case err == nil:
		if existingKey == in.IdempotencyKey && existingCase != in.CaseID {
			return nil, fmt.Errorf("%w: idempotency key already used for another case", ErrConflict)
		}
		if existingLabel != in.Label {
			return nil, fmt.Errorf("%w: case already labelled %s", ErrConflict, existingLabel)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return s.GetCase(ctx, in.CaseID)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	var note any
	if in.Note != "" {
		note = in.Note
	}
	labelID := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO analyst_labels (label_id, case_id, label, note, analyst_sub, idempotency_key) VALUES ($1, $2::uuid, $3, $4, $5, $6)`,
		labelID, in.CaseID, in.Label, note, in.ActorSub, in.IdempotencyKey); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE review_cases SET status = 'labelled', labelled_at = now() WHERE case_id = $1::uuid`, in.CaseID); err != nil {
		return nil, err
	}
	if _, err := AppendAudit(ctx, tx, in.ActorSub, in.Role, "case.label", "case/"+in.CaseID,
		map[string]any{"label": in.Label, "label_id": labelID, "idempotency_key": in.IdempotencyKey, "note_chars": len([]rune(in.Note))}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetCase(ctx, in.CaseID)
}

var eventTitles = map[string]string{"login": "Login", "device": "Device change", "transaction": "Transaction"}

// Timeline returns the account's events, decisions and labels, newest first.
func (s *CaseStore) Timeline(ctx context.Context, accountID string, limit int) ([]*fraudv1.TimelineItem, error) {
	rows, err := s.pool.Query(ctx, `
		(SELECT 'event.' || kind, event_id, event_ts, payload::text FROM account_events WHERE account_id = $1 ORDER BY event_ts DESC LIMIT $2)
		UNION ALL
		(SELECT 'decision', decision_id::text, decided_at,
		        json_build_object('action', action, 'score', score, 'reason_codes', array_to_string(reason_codes, ','), 'model_version', model_version,
		                          'transaction_id', transaction_id)::text
		   FROM decisions WHERE account_id = $1 ORDER BY decided_at DESC LIMIT $2)
		UNION ALL
		(SELECT 'label', al.label_id::text, al.created_at,
		        json_build_object('label', al.label, 'analyst', al.analyst_sub, 'case_id', al.case_id)::text
		   FROM analyst_labels al JOIN review_cases rc ON rc.case_id = al.case_id WHERE rc.account_id = $1 ORDER BY al.created_at DESC LIMIT $2)
		ORDER BY 3 DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*fraudv1.TimelineItem
	for rows.Next() {
		var kind, id, payload string
		var ts time.Time
		if err := rows.Scan(&kind, &id, &ts, &payload); err != nil {
			return nil, err
		}
		var obj map[string]any
		_ = json.Unmarshal([]byte(payload), &obj)
		details := map[string]string{}
		for k, v := range obj {
			if v == nil || k == "schema" || k == "event_ts" {
				continue
			}
			switch x := v.(type) {
			case string:
				details[k] = x
			case float64:
				details[k] = FormatFactNumber(x)
			default:
				details[k] = fmt.Sprint(x)
			}
		}
		title := kind
		switch {
		case strings.HasPrefix(kind, "event."):
			title = eventTitles[strings.TrimPrefix(kind, "event.")]
			if c, ok := details["change"]; ok {
				title += ": " + c
			}
			if a, ok := details["amount_minor"]; ok {
				title += " " + a + " " + details["currency"]
			}
		case kind == "decision":
			title = "Decision " + details["action"]
		case kind == "label":
			title = "Labelled " + details["label"]
		}
		out = append(out, &fraudv1.TimelineItem{Kind: kind, Id: id, Ts: timestamppb.New(ts), Title: title, Details: details})
	}
	return out, rows.Err()
}

// ShadowSummary aggregates comparisons over the last windowHours.
func (s *CaseStore) ShadowSummary(ctx context.Context, windowHours, limit int) (*fraudv1.ShadowSummary, error) {
	out := &fraudv1.ShadowSummary{}
	since := time.Now().Add(-time.Duration(windowHours) * time.Hour)
	var agree int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE agree) FROM shadow_comparisons WHERE created_at > $1`, since).Scan(&out.Total, &agree); err != nil {
		return nil, err
	}
	if out.Total > 0 {
		out.AgreementRate = float64(agree) / float64(out.Total)
	}
	err := s.pool.QueryRow(ctx, `SELECT primary_version, shadow_version FROM shadow_comparisons WHERE created_at > $1 ORDER BY created_at DESC LIMIT 1`, since).
		Scan(&out.PrimaryVersion, &out.ShadowVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT primary_band, shadow_band, count(*) FROM shadow_comparisons WHERE created_at > $1 GROUP BY 1, 2 ORDER BY 1, 2`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c fraudv1.BandCell
		if err := rows.Scan(&c.PrimaryBand, &c.ShadowBand, &c.Count); err != nil {
			rows.Close()
			return nil, err
		}
		out.BandMatrix = append(out.BandMatrix, &c)
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT decision_id::text, primary_score, shadow_score, primary_band, shadow_band, created_at
		FROM shadow_comparisons WHERE created_at > $1 AND NOT agree ORDER BY created_at DESC LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c fraudv1.ShadowComparison
		var ts time.Time
		if err := rows.Scan(&c.DecisionId, &c.PrimaryScore, &c.ShadowScore, &c.PrimaryBand, &c.ShadowBand, &ts); err != nil {
			return nil, err
		}
		c.CreatedAt = timestamppb.New(ts)
		out.Disagreements = append(out.Disagreements, &c)
	}
	return out, rows.Err()
}

// FeatureSample is one recent decision's rule environment (TestRule).
type FeatureSample struct {
	DecisionID string
	Features   map[string]float64
	Tx         TxAttrs
}

// RecentSamples returns the n most recent decisions' features and attributes.
func (s *CaseStore) RecentSamples(ctx context.Context, n int) ([]FeatureSample, error) {
	rows, err := s.pool.Query(ctx, `SELECT decision_id::text, features, tx FROM decisions ORDER BY decided_at DESC LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeatureSample
	for rows.Next() {
		var fs FeatureSample
		var f, t []byte
		if err := rows.Scan(&fs.DecisionID, &f, &t); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(f, &fs.Features)
		_ = json.Unmarshal(t, &fs.Tx)
		out = append(out, fs)
	}
	return out, rows.Err()
}

// SaveRuleSet records a loaded rule set and marks it the only active one.
func (s *CaseStore) SaveRuleSet(ctx context.Context, version, sha string, body []byte) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `UPDATE rule_sets SET active = false WHERE active AND version <> $1`, version); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO rule_sets (version, sha256, body, loaded_at, active) VALUES ($1, $2, $3, now(), true)
		ON CONFLICT (version) DO UPDATE SET sha256 = EXCLUDED.sha256, body = EXCLUDED.body, loaded_at = now(), active = true`, version, sha, string(body)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
