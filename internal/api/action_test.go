package api

import (
	"testing"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/rules"
)

func TestProtoActionIsCheckedNotCast(t *testing.T) {
	for in, want := range map[rules.Action]fraudv1.Action{
		rules.Approve: fraudv1.Action_ACTION_APPROVE, rules.Review: fraudv1.Action_ACTION_REVIEW,
		rules.Decline: fraudv1.Action_ACTION_DECLINE, rules.Unspecified: fraudv1.Action_ACTION_UNSPECIFIED,
		rules.Action(1 << 40): fraudv1.Action_ACTION_UNSPECIFIED, rules.Action(-1): fraudv1.Action_ACTION_UNSPECIFIED,
	} {
		if got := protoAction(in); got != want {
			t.Fatalf("protoAction(%d) = %v, want %v", in, got, want)
		}
	}
}
