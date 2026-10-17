package api

import (
	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/rules"
)

// protoAction maps a rule-engine action to the wire enum. It is checked rather than cast: an out-of-range
// value becomes ACTION_UNSPECIFIED instead of an arbitrary int32 (gosec G115).
func protoAction(a rules.Action) fraudv1.Action {
	switch a {
	case rules.Approve:
		return fraudv1.Action_ACTION_APPROVE
	case rules.Review:
		return fraudv1.Action_ACTION_REVIEW
	case rules.Decline:
		return fraudv1.Action_ACTION_DECLINE
	default:
		return fraudv1.Action_ACTION_UNSPECIFIED
	}
}
