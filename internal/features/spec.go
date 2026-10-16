// Package features computes point-in-time online features in Redis (features/spec.yaml v1).
package features

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	featurespec "github.com/ROBERTG99/fraud-scoring-service/features"
)

// NumFeatures is the model input width.
const NumFeatures = 13

// Feature indexes in the model input order.
const (
	IdxTxCount10m = iota
	IdxTxCount1h
	IdxTxCount24h
	IdxAmountSum1h
	IdxAmountMinor
	IdxDistinctDevices24h
	IdxNewDevice
	IdxMinutesSincePwd
	IdxMinutesSinceEmail
	IdxMCCNovel30d
	IdxChannel
	IdxHourOfDay
	IdxAmountRatio24h
)

// Window lengths in milliseconds.
const (
	Ms10m = int64(600_000)
	Ms1h  = int64(3_600_000)
	Ms24h = int64(86_400_000)
	Ms30d = 30 * Ms24h
	Ms90d = 90 * Ms24h
	// MinutesCap is the value used when no credential change is known.
	MinutesCap = 43_200
)

// Vector is one feature row in spec order.
type Vector [NumFeatures]float64

// Spec is the parsed feature spec.
type Spec struct {
	Schema       string           `yaml:"schema"`
	Version      string           `yaml:"version"`
	ChannelCodes map[string]int   `yaml:"channel_codes"`
	Features     []FeatureDef     `yaml:"features"`
	Fallback     map[string]int64 `yaml:"fallback_defaults"`
	RetentionMs  map[string]int64 `yaml:"retention_ms"`
}

// FeatureDef is one entry of spec.features.
type FeatureDef struct {
	Name     string `yaml:"name"`
	Kind     string `yaml:"kind"`
	WindowMs int64  `yaml:"window_ms"`
	Cap      int64  `yaml:"cap"`
}

var expectedNames = [NumFeatures]string{
	"tx_count_10m", "tx_count_1h", "tx_count_24h", "amount_sum_1h_minor", "amount_minor",
	"distinct_devices_24h", "new_device", "minutes_since_pwd_change", "minutes_since_email_change",
	"mcc_novel_30d", "channel", "hour_of_day_utc", "amount_ratio_24h",
}

// Names returns the feature names in model input order.
func Names() []string { return expectedNames[:] }

// ParseSpec parses and validates a spec document.
func ParseSpec(raw []byte) (*Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("feature spec: %w", err)
	}
	if s.Version != "v1" || len(s.Features) != NumFeatures {
		return nil, fmt.Errorf("feature spec: want version v1 with %d features, got %s/%d", NumFeatures, s.Version, len(s.Features))
	}
	for i, f := range s.Features {
		if f.Name != expectedNames[i] {
			return nil, fmt.Errorf("feature spec: feature %d is %q, want %q", i, f.Name, expectedNames[i])
		}
	}
	for _, c := range []string{"CARD_PRESENT", "ECOMMERCE", "P2P"} {
		if s.ChannelCodes[c] == 0 {
			return nil, fmt.Errorf("feature spec: missing channel code %s", c)
		}
	}
	return &s, nil
}

// DefaultSpec returns the embedded spec.
func DefaultSpec() *Spec {
	s, err := ParseSpec(featurespec.SpecYAML)
	if err != nil {
		panic(err)
	}
	return s
}

// LoadSpec reads a spec from path, or the embedded one when path is empty.
func LoadSpec(path string) (*Spec, error) {
	if path == "" {
		return DefaultSpec(), nil
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-provided config path
	if err != nil {
		return nil, err
	}
	return ParseSpec(raw)
}

// ChannelCode maps an event channel ("ECOMMERCE") to its numeric code.
func (s *Spec) ChannelCode(channel string) float64 { return float64(s.ChannelCodes[channel]) }

// Map returns the vector as name → value (for explain=true and persistence).
func (v *Vector) Map() map[string]float64 {
	m := make(map[string]float64, NumFeatures)
	for i, n := range expectedNames {
		m[n] = v[i]
	}
	return m
}

// Float32 returns the model input row.
func (v *Vector) Float32() []float32 {
	out := make([]float32, NumFeatures)
	for i, x := range v {
		out[i] = float32(x)
	}
	return out
}
