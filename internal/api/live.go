package api

import (
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
)

// LiveStats keeps per-second buckets of recent decisions for StreamLiveMetrics (RPS, p95, action mix,
// degraded rate). Memory is bounded: 60 buckets of fixed size.
type LiveStats struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets [60]liveBucket
	// UpdaterLag returns the feature-updater lag in seconds (optional).
	UpdaterLag func() float64
}

type liveBucket struct {
	sec                 int64
	count               int64
	approve             int64
	review              int64
	decline             int64
	degraded            int64
	modelUnavailable    int64
	featuresUnavailable int64
	latencies           []float32 // ms, capped per bucket
}

const maxLatencySamples = 2048

// NewLiveStats returns an empty aggregator.
func NewLiveStats(now func() time.Time) *LiveStats {
	if now == nil {
		now = time.Now
	}
	return &LiveStats{now: now}
}

// Record adds one decision.
func (l *LiveStats) Record(d *fraudv1.Decision, latency time.Duration) {
	if l == nil {
		return
	}
	sec := l.now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := &l.buckets[sec%int64(len(l.buckets))]
	if b.sec != sec {
		*b = liveBucket{sec: sec, latencies: b.latencies[:0]}
	}
	b.count++
	switch d.Action {
	case fraudv1.Action_ACTION_APPROVE:
		b.approve++
	case fraudv1.Action_ACTION_REVIEW:
		b.review++
	case fraudv1.Action_ACTION_DECLINE:
		b.decline++
	}
	if d.Degraded {
		b.degraded++
	}
	for _, c := range d.ReasonCodes {
		switch c {
		case "MODEL_UNAVAILABLE":
			b.modelUnavailable++
		case "FEATURES_UNAVAILABLE":
			b.featuresUnavailable++
		}
	}
	if len(b.latencies) < maxLatencySamples {
		b.latencies = append(b.latencies, float32(latency.Seconds()*1000))
	}
}

// Snapshot summarises the last window (excluding the current, still filling second).
func (l *LiveStats) Snapshot(window time.Duration) *fraudv1.LiveMetrics {
	now := l.now()
	secs := int64(window / time.Second)
	if secs < 1 {
		secs = 1
	}
	end := now.Unix() - 1
	out := &fraudv1.LiveMetrics{Ts: timestamppb.New(now)}
	var lat []float32
	var count, degraded int64
	l.mu.Lock()
	for s := end - secs + 1; s <= end; s++ {
		b := &l.buckets[((s%60)+60)%60]
		if b.sec != s {
			continue
		}
		count += b.count
		out.Approve += b.approve
		out.Review += b.review
		out.Decline += b.decline
		degraded += b.degraded
		out.ModelUnavailable += b.modelUnavailable
		out.FeaturesUnavailable += b.featuresUnavailable
		lat = append(lat, b.latencies...)
	}
	l.mu.Unlock()
	out.Rps = float64(count) / float64(secs)
	if count > 0 {
		out.DegradedRate = float64(degraded) / float64(count)
	}
	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		idx := int(float64(len(lat))*0.95+0.5) - 1
		out.P95Ms = float64(lat[min(max(idx, 0), len(lat)-1)])
	}
	if l.UpdaterLag != nil {
		out.UpdaterLagSeconds = l.UpdaterLag()
	}
	return out
}
