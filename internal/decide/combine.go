// Package decide combines rule matches with the model band into the final action.
package decide

import (
	"sort"

	"github.com/ROBERTG99/fraud-scoring-service/internal/rules"
)

// Degradation reason codes.
const (
	ReasonModelUnavailable    = "MODEL_UNAVAILABLE"
	ReasonFeaturesUnavailable = "FEATURES_UNAVAILABLE"
)

// Input to Combine.
type Input struct {
	Matches     []rules.Match
	Score       float64 // calibrated probability; ignored unless ModelOK && FeaturesOK
	ModelOK     bool
	FeaturesOK  bool
	AmountMinor int64
}

// Result of Combine.
type Result struct {
	Action      rules.Action
	ReasonCodes []string
	Degraded    bool
}

// BandOf maps a calibrated score to its threshold band.
func BandOf(rs *rules.RuleSet, score float64) string {
	return modelBand(rs, score).String()
}

func modelBand(rs *rules.RuleSet, score float64) rules.Action {
	switch {
	case score >= rs.Thresholds.Decline:
		return rules.Decline
	case score >= rs.Thresholds.Review:
		return rules.Review
	default:
		return rules.Approve
	}
}

// Combine applies the threshold table:
//   - severity APPROVE < REVIEW < DECLINE;
//   - a matched final rule wins (the most severe one if several);
//   - otherwise action = max(matched rule actions, model band);
//   - in degraded mode the model band is replaced by REVIEW if amount_minor >= review_min_amount_minor
//     (the lower floor when both model and features are unavailable) else APPROVE.
//
// Reason codes: final rule, other rules by ID, model code, degradation codes.
func Combine(rs *rules.RuleSet, in Input) Result {
	degraded := !in.ModelOK || !in.FeaturesOK
	var band rules.Action
	if degraded {
		floor := int64(0)
		switch {
		case !in.ModelOK && !in.FeaturesOK:
			floor = min(rs.Degraded.ModelUnavailable.ReviewMinAmountMinor, rs.Degraded.FeaturesUnavailable.ReviewMinAmountMinor)
		case !in.ModelOK:
			floor = rs.Degraded.ModelUnavailable.ReviewMinAmountMinor
		default:
			floor = rs.Degraded.FeaturesUnavailable.ReviewMinAmountMinor
		}
		band = rules.Approve
		if in.AmountMinor >= floor {
			band = rules.Review
		}
	} else {
		band = modelBand(rs, in.Score)
	}

	var final *rules.Match
	others := make([]rules.Match, 0, len(in.Matches))
	for i := range in.Matches {
		m := in.Matches[i]
		if m.Final && (final == nil || m.Action > final.Action) {
			if final != nil {
				others = append(others, *final)
			}
			final = &in.Matches[i]
			continue
		}
		others = append(others, m)
	}
	sort.SliceStable(others, func(i, j int) bool { return others[i].ID < others[j].ID })

	action := band
	if final != nil {
		action = final.Action
	} else {
		for _, m := range others {
			action = max(action, m.Action)
		}
	}

	reasons := make([]string, 0, len(in.Matches)+2)
	seen := map[string]bool{}
	add := func(code string) {
		if code != "" && !seen[code] {
			seen[code] = true
			reasons = append(reasons, code)
		}
	}
	if final != nil {
		add(final.ReasonCode)
	}
	for _, m := range others {
		add(m.ReasonCode)
	}
	if !degraded {
		code := ""
		for _, mc := range rs.ModelReasonCodes {
			if in.Score >= mc.MinScore && (code == "" || mc.MinScore >= minScoreOf(rs, code)) {
				code = mc.Code
			}
		}
		add(code)
	}
	if !in.ModelOK {
		add(ReasonModelUnavailable)
	}
	if !in.FeaturesOK {
		add(ReasonFeaturesUnavailable)
	}
	if len(reasons) == 0 {
		reasons = nil
	}
	return Result{Action: action, ReasonCodes: reasons, Degraded: degraded}
}

func minScoreOf(rs *rules.RuleSet, code string) float64 {
	for _, mc := range rs.ModelReasonCodes {
		if mc.Code == code {
			return mc.MinScore
		}
	}
	return 0
}
