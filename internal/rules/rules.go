// Package rules loads versioned YAML rule sets (fraud.rules.v1), compiles them with expr-lang/expr
// against a typed environment, and evaluates them on the scoring path.
package rules

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/schemas"
)

// MaxRules bounds a rule set (spec §4.4).
const MaxRules = 64

// Action mirrors fraud.v1.Action values.
type Action int

// Actions in severity order (APPROVE < REVIEW < DECLINE).
const (
	Unspecified Action = 0
	Approve     Action = 1
	Review      Action = 2
	Decline     Action = 3
)

func (a Action) String() string {
	switch a {
	case Approve:
		return "APPROVE"
	case Review:
		return "REVIEW"
	case Decline:
		return "DECLINE"
	default:
		return "UNSPECIFIED"
	}
}

// ParseAction parses APPROVE|REVIEW|DECLINE.
func ParseAction(s string) (Action, error) {
	switch s {
	case "APPROVE":
		return Approve, nil
	case "REVIEW":
		return Review, nil
	case "DECLINE":
		return Decline, nil
	}
	return Unspecified, fmt.Errorf("unknown action %q", s)
}

// Env is the typed environment rules are compiled against. Field names in expressions are the
// feature names of features/spec.yaml plus request attributes (mcc, channel, country, currency).
type Env struct {
	TxCount10m              float64 `expr:"tx_count_10m"`
	TxCount1h               float64 `expr:"tx_count_1h"`
	TxCount24h              float64 `expr:"tx_count_24h"`
	AmountSum1hMinor        float64 `expr:"amount_sum_1h_minor"`
	AmountMinor             float64 `expr:"amount_minor"`
	DistinctDevices24h      float64 `expr:"distinct_devices_24h"`
	NewDevice               bool    `expr:"new_device"`
	MinutesSincePwdChange   float64 `expr:"minutes_since_pwd_change"`
	MinutesSinceEmailChange float64 `expr:"minutes_since_email_change"`
	MCCNovel30d             bool    `expr:"mcc_novel_30d"`
	HourOfDayUTC            float64 `expr:"hour_of_day_utc"`
	AmountRatio24h          float64 `expr:"amount_ratio_24h"`
	MCC                     string  `expr:"mcc"`
	Channel                 string  `expr:"channel"`
	Country                 string  `expr:"country"`
	Currency                string  `expr:"currency"`
}

// EnvFromVector builds the rule environment from a feature vector and request attributes.
func EnvFromVector(v *features.Vector, mcc, channel, country, currency string) Env {
	return Env{
		TxCount10m: v[features.IdxTxCount10m], TxCount1h: v[features.IdxTxCount1h], TxCount24h: v[features.IdxTxCount24h],
		AmountSum1hMinor: v[features.IdxAmountSum1h], AmountMinor: v[features.IdxAmountMinor],
		DistinctDevices24h: v[features.IdxDistinctDevices24h], NewDevice: v[features.IdxNewDevice] >= 1,
		MinutesSincePwdChange: v[features.IdxMinutesSincePwd], MinutesSinceEmailChange: v[features.IdxMinutesSinceEmail],
		MCCNovel30d: v[features.IdxMCCNovel30d] >= 1, HourOfDayUTC: v[features.IdxHourOfDay],
		AmountRatio24h: v[features.IdxAmountRatio24h], MCC: mcc, Channel: channel, Country: country, Currency: currency,
	}
}

// EnvFromMap builds an environment from stored features (decisions.features jsonb) for TestRule.
func EnvFromMap(m map[string]float64, mcc, channel, country, currency string) Env {
	var v features.Vector
	for i, n := range features.Names() {
		v[i] = m[n]
	}
	return EnvFromVector(&v, mcc, channel, country, currency)
}

// Rule is one compiled rule.
type Rule struct {
	ID          string
	Name        string
	When        string
	Action      Action
	ReasonCode  string
	Final       bool
	Description string
	program     *vm.Program
}

// Match is a rule that fired.
type Match struct {
	ID         string
	ReasonCode string
	Action     Action
	Final      bool
}

// ModelCode adds a reason code when the model score reaches MinScore.
type ModelCode struct {
	MinScore float64 `yaml:"min_score" json:"min_score"`
	Code     string  `yaml:"code" json:"code"`
}

// Thresholds are the calibrated probability bands.
type Thresholds struct {
	Review  float64 `yaml:"review" json:"review"`
	Decline float64 `yaml:"decline" json:"decline"`
}

// Degraded holds the amount floors used when the model or features are unavailable.
type Degraded struct {
	ModelUnavailable    DegradedBand `yaml:"model_unavailable" json:"model_unavailable"`
	FeaturesUnavailable DegradedBand `yaml:"features_unavailable" json:"features_unavailable"`
}

// DegradedBand is REVIEW if amount_minor >= ReviewMinAmountMinor else APPROVE.
type DegradedBand struct {
	ReviewMinAmountMinor int64 `yaml:"review_min_amount_minor" json:"review_min_amount_minor"`
}

// RuleSet is an immutable compiled rule set.
type RuleSet struct {
	Version          string
	SHA256           string
	FeatureSpec      string
	Thresholds       Thresholds
	Degraded         Degraded
	ModelReasonCodes []ModelCode
	Rules            []Rule
	Body             json.RawMessage
	LoadedAt         time.Time
}

type rawRule struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	When        string `yaml:"when" json:"when"`
	Action      string `yaml:"action" json:"action"`
	ReasonCode  string `yaml:"reason_code" json:"reason_code"`
	Final       bool   `yaml:"final,omitempty" json:"final,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

type rawSet struct {
	Schema           string      `yaml:"schema" json:"schema"`
	Version          string      `yaml:"version" json:"version"`
	FeatureSpec      string      `yaml:"feature_spec" json:"feature_spec"`
	Thresholds       Thresholds  `yaml:"thresholds" json:"thresholds"`
	Degraded         Degraded    `yaml:"degraded" json:"degraded"`
	ModelReasonCodes []ModelCode `yaml:"model_reason_codes" json:"model_reason_codes"`
	Rules            []rawRule   `yaml:"rules" json:"rules"`
}

var (
	schemaOnce sync.Once
	schema     *jsonschema.Schema
	schemaErr  error
)

func rulesSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		raw, err := fs.ReadFile(schemas.FS, "rules.v1.json")
		if err != nil {
			schemaErr = err
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			schemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("mem://rules.v1.json", doc); err != nil {
			schemaErr = err
			return
		}
		schema, schemaErr = c.Compile("mem://rules.v1.json")
	})
	return schema, schemaErr
}

// CompileExpr compiles a boolean rule expression against Env.
func CompileExpr(when string) (*vm.Program, error) {
	return expr.Compile(when, expr.Env(Env{}), expr.AsBool(), expr.Optimize(true))
}

// Parse validates (JSON Schema), compiles and returns a rule set. Any error rejects the whole set.
func Parse(raw []byte) (*RuleSet, error) {
	var generic any
	if err := yaml.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("rules: yaml: %w", err)
	}
	asJSON, err := json.Marshal(generic)
	if err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	s, err := rulesSchema()
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	if err != nil {
		return nil, err
	}
	if err := s.Validate(doc); err != nil {
		return nil, fmt.Errorf("rules: schema: %w", err)
	}
	var rs rawSet
	if err := json.Unmarshal(asJSON, &rs); err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	if len(rs.Rules) > MaxRules {
		return nil, fmt.Errorf("rules: %d rules exceed the limit of %d", len(rs.Rules), MaxRules)
	}
	if rs.Thresholds.Review >= rs.Thresholds.Decline {
		return nil, errors.New("rules: thresholds.review must be below thresholds.decline")
	}
	sum := sha256.Sum256(raw)
	out := &RuleSet{Version: rs.Version, SHA256: hex.EncodeToString(sum[:]), FeatureSpec: rs.FeatureSpec,
		Thresholds: rs.Thresholds, Degraded: rs.Degraded, ModelReasonCodes: rs.ModelReasonCodes, Body: asJSON, LoadedAt: time.Now().UTC()}
	seen := map[string]bool{}
	for _, r := range rs.Rules {
		if seen[r.ID] {
			return nil, fmt.Errorf("rules: duplicate id %s", r.ID)
		}
		seen[r.ID] = true
		act, err := ParseAction(r.Action)
		if err != nil {
			return nil, err
		}
		prog, err := CompileExpr(r.When)
		if err != nil {
			return nil, fmt.Errorf("rules: %s: %w", r.ID, err)
		}
		out.Rules = append(out.Rules, Rule{ID: r.ID, Name: r.Name, When: r.When, Action: act, ReasonCode: r.ReasonCode,
			Final: r.Final, Description: r.Description, program: prog})
	}
	return out, nil
}

// Evaluate returns the matching rules in rule-set order. A rule that errors at runtime does not match.
func (rs *RuleSet) Evaluate(env *Env) []Match {
	var out []Match
	for i := range rs.Rules {
		r := &rs.Rules[i]
		v, err := vm.Run(r.program, env)
		if err != nil {
			continue
		}
		if b, ok := v.(bool); ok && b {
			out = append(out, Match{ID: r.ID, ReasonCode: r.ReasonCode, Action: r.Action, Final: r.Final})
		}
	}
	return out
}

// EvalProgram runs a compiled expression (TestRule).
func EvalProgram(p *vm.Program, env *Env) (bool, error) {
	v, err := vm.Run(p, env)
	if err != nil {
		return false, err
	}
	b, _ := v.(bool)
	return b, nil
}
