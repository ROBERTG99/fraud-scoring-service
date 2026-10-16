package api

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/rules"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
)

// ConsoleTimeout bounds every console call (spec §4.7).
const ConsoleTimeout = 2 * time.Second

// LiveInterval is the StreamLiveMetrics push period.
const LiveInterval = 2 * time.Second

// CaseBackend is the storage the CaseService needs (store.CaseStore in production).
type CaseBackend interface {
	ListCases(ctx context.Context, q store.ListQuery) ([]*fraudv1.Case, string, int32, error)
	GetCase(ctx context.Context, caseID string) (*fraudv1.Case, error)
	LabelCase(ctx context.Context, in store.LabelInput) (*fraudv1.Case, error)
	Timeline(ctx context.Context, accountID string, limit int) ([]*fraudv1.TimelineItem, error)
	ShadowSummary(ctx context.Context, windowHours, limit int) (*fraudv1.ShadowSummary, error)
	RecentSamples(ctx context.Context, n int) ([]store.FeatureSample, error)
}

// CaseServer implements fraud.v1.CaseService.
type CaseServer struct {
	fraudv1.UnimplementedCaseServiceServer
	Backend       CaseBackend
	Rules         *rules.Engine
	Live          *LiveStats
	ShadowDropped func() int64
	Log           *slog.Logger
	LiveEvery     time.Duration
}

var (
	reIdemKey    = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
	reReasonCode = regexp.MustCompile(`^[A-Z0-9_]{0,64}$`)
)

func fieldErr(field, desc string) error {
	return invalidArgument([]*errdetails.BadRequest_FieldViolation{{Field: field, Description: desc}})
}

func (s *CaseServer) mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return status.Error(codes.NotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, store.ErrBadPageToken):
		return fieldErr("page_token", "invalid")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	}
	if s.Log != nil {
		s.Log.Error("case service", "err", err)
	}
	return status.Error(codes.Internal, "internal error")
}

// validUUID accepts the canonical lowercase form (the OpenAPI pattern).
func validUUID(s string) bool { return reUUID.MatchString(s) }

// ListCases returns open (or labelled) cases by priority.
func (s *CaseServer) ListCases(ctx context.Context, req *fraudv1.ListCasesRequest) (*fraudv1.ListCasesResponse, error) {
	size := int(req.PageSize)
	switch {
	case size == 0:
		size = 50
	case size < 0 || size > 100:
		return nil, fieldErr("page_size", "must be between 1 and 100")
	}
	st := "open"
	switch req.Status {
	case fraudv1.CaseStatus_CASE_STATUS_UNSPECIFIED, fraudv1.CaseStatus_CASE_STATUS_OPEN:
	case fraudv1.CaseStatus_CASE_STATUS_LABELLED:
		st = "labelled"
	default:
		return nil, fieldErr("status", "unknown status")
	}
	switch req.SummaryStatus {
	case "", "pending", "ok", "fallback", "unfaithful":
	default:
		return nil, fieldErr("summary_status", "must be pending, ok, fallback or unfaithful")
	}
	if !reReasonCode.MatchString(req.ReasonCode) {
		return nil, fieldErr("reason_code", "must be an upper-case reason code")
	}
	if len(req.PageToken) > 256 {
		return nil, fieldErr("page_token", "invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, ConsoleTimeout)
	defer cancel()
	cases, next, open, err := s.Backend.ListCases(ctx, store.ListQuery{Status: st, ReasonCode: req.ReasonCode, SummaryStatus: req.SummaryStatus, PageSize: size, PageToken: req.PageToken})
	if err != nil {
		return nil, s.mapErr(err)
	}
	return &fraudv1.ListCasesResponse{Cases: cases, NextPageToken: next, TotalOpen: open}, nil
}

// GetCase returns one case.
func (s *CaseServer) GetCase(ctx context.Context, req *fraudv1.GetCaseRequest) (*fraudv1.Case, error) {
	if !validUUID(req.CaseId) {
		return nil, fieldErr("case_id", "must be a UUID")
	}
	ctx, cancel := context.WithTimeout(ctx, ConsoleTimeout)
	defer cancel()
	c, err := s.Backend.GetCase(ctx, req.CaseId)
	return c, s.mapErr(err)
}

// LabelCase writes an analyst label (analyst only, enforced by RBAC).
func (s *CaseServer) LabelCase(ctx context.Context, req *fraudv1.LabelCaseRequest) (*fraudv1.Case, error) {
	var v []*errdetails.BadRequest_FieldViolation
	if !validUUID(req.CaseId) {
		v = append(v, &errdetails.BadRequest_FieldViolation{Field: "case_id", Description: "must be a UUID"})
	}
	label := ""
	switch req.Label {
	case fraudv1.Label_LABEL_FRAUD:
		label = "fraud"
	case fraudv1.Label_LABEL_LEGIT:
		label = "legit"
	default:
		v = append(v, &errdetails.BadRequest_FieldViolation{Field: "label", Description: "must be LABEL_FRAUD or LABEL_LEGIT"})
	}
	if utf8.RuneCountInString(req.Note) > 2000 || !utf8.ValidString(req.Note) || strings.ContainsRune(req.Note, 0) {
		v = append(v, &errdetails.BadRequest_FieldViolation{Field: "note", Description: "at most 2000 characters, no NUL"})
	}
	if !reIdemKey.MatchString(req.IdempotencyKey) {
		v = append(v, &errdetails.BadRequest_FieldViolation{Field: "idempotency_key", Description: "8-128 characters of [A-Za-z0-9_-]"})
	}
	if len(v) > 0 {
		return nil, invalidArgument(v)
	}
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no principal")
	}
	ctx, cancel := context.WithTimeout(ctx, ConsoleTimeout)
	defer cancel()
	c, err := s.Backend.LabelCase(ctx, store.LabelInput{CaseID: req.CaseId, Label: label, Note: req.Note, IdempotencyKey: req.IdempotencyKey, ActorSub: p.Subject, Role: p.Role})
	return c, s.mapErr(err)
}

var reAccountID = regexp.MustCompile(`^acc_[0-9a-z]{6,32}$`)

// GetTimeline returns the account's activity.
func (s *CaseServer) GetTimeline(ctx context.Context, req *fraudv1.GetTimelineRequest) (*fraudv1.Timeline, error) {
	if !reAccountID.MatchString(req.AccountId) {
		return nil, fieldErr("account_id", "must be a synthetic account token")
	}
	limit := int(req.Limit)
	switch {
	case limit == 0:
		limit = 100
	case limit < 0 || limit > 200:
		return nil, fieldErr("limit", "must be between 1 and 200")
	}
	ctx, cancel := context.WithTimeout(ctx, ConsoleTimeout)
	defer cancel()
	items, err := s.Backend.Timeline(ctx, req.AccountId, limit)
	if err != nil {
		return nil, s.mapErr(err)
	}
	return &fraudv1.Timeline{AccountId: req.AccountId, Items: items}, nil
}

// GetShadowSummary compares primary and shadow models.
func (s *CaseServer) GetShadowSummary(ctx context.Context, req *fraudv1.GetShadowSummaryRequest) (*fraudv1.ShadowSummary, error) {
	hours, limit := int(req.WindowHours), int(req.DisagreementLimit)
	if hours == 0 {
		hours = 24
	}
	if limit == 0 {
		limit = 50
	}
	if hours < 0 || hours > 720 {
		return nil, fieldErr("window_hours", "must be between 1 and 720")
	}
	if limit < 0 || limit > 100 {
		return nil, fieldErr("disagreement_limit", "must be between 1 and 100")
	}
	ctx, cancel := context.WithTimeout(ctx, ConsoleTimeout)
	defer cancel()
	sum, err := s.Backend.ShadowSummary(ctx, hours, limit)
	if err != nil {
		return nil, s.mapErr(err)
	}
	if s.ShadowDropped != nil {
		sum.Dropped = s.ShadowDropped()
	}
	return sum, nil
}

// GetActiveRuleSet returns the rule set the scorer is using now.
func (s *CaseServer) GetActiveRuleSet(context.Context, *fraudv1.GetActiveRuleSetRequest) (*fraudv1.RuleSet, error) {
	rs := s.Rules.Active()
	out := &fraudv1.RuleSet{Version: rs.Version, Sha256: rs.SHA256, FeatureSpec: rs.FeatureSpec, LoadedAt: timestamppb.New(rs.LoadedAt),
		Thresholds: &fraudv1.Thresholds{Review: rs.Thresholds.Review, Decline: rs.Thresholds.Decline}}
	for _, r := range rs.Rules {
		out.Rules = append(out.Rules, &fraudv1.Rule{Id: r.ID, Name: r.Name, When: r.When, Action: fraudv1.Action(r.Action), ReasonCode: r.ReasonCode, Final: r.Final})
	}
	return out, nil
}

// TestRule evaluates a candidate expression against recent decisions without activating it.
func (s *CaseServer) TestRule(ctx context.Context, req *fraudv1.TestRuleRequest) (*fraudv1.TestRuleResponse, error) {
	if req.When == "" || utf8.RuneCountInString(req.When) > 500 {
		return nil, fieldErr("when", "must be 1 to 500 characters")
	}
	n := int(req.SampleSize)
	switch {
	case n == 0:
		n = 1000
	case n < 0 || n > 5000:
		return nil, fieldErr("sample_size", "must be between 1 and 5000")
	}
	prog, err := rules.CompileExpr(req.When)
	if err != nil {
		return &fraudv1.TestRuleResponse{Valid: false, Error: err.Error()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, ConsoleTimeout)
	defer cancel()
	samples, err := s.Backend.RecentSamples(ctx, n)
	if err != nil {
		return nil, s.mapErr(err)
	}
	out := &fraudv1.TestRuleResponse{Valid: true, Evaluated: int64(len(samples))}
	for _, smp := range samples {
		env := rules.EnvFromMap(smp.Features, smp.Tx.MCC, smp.Tx.Channel, smp.Tx.Country, smp.Tx.Currency)
		ok, err := rules.EvalProgram(prog, &env)
		if err != nil {
			continue
		}
		if ok {
			out.Matched++
			if len(out.SampleDecisionIds) < 10 {
				out.SampleDecisionIds = append(out.SampleDecisionIds, smp.DecisionID)
			}
		}
	}
	if out.Evaluated > 0 {
		out.MatchRate = float64(out.Matched) / float64(out.Evaluated)
	}
	return out, nil
}

// StreamLiveMetrics pushes a snapshot immediately and then every 2 s until the client goes away.
func (s *CaseServer) StreamLiveMetrics(_ *fraudv1.StreamLiveMetricsRequest, stream fraudv1.CaseService_StreamLiveMetricsServer) error {
	every := s.LiveEvery
	if every <= 0 {
		every = LiveInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := stream.Send(s.Live.Snapshot(10 * time.Second)); err != nil {
			return err
		}
		select {
		case <-stream.Context().Done():
			return nil
		case <-t.C:
		}
	}
}
