package features

import (
	"testing"
)

func TestSpecLoadsAndMatchesOrder(t *testing.T) {
	s := DefaultSpec()
	if len(s.Features) != NumFeatures || s.Features[IdxAmountRatio24h].Name != "amount_ratio_24h" {
		t.Fatalf("unexpected spec: %+v", s.Features)
	}
	if s.ChannelCode("P2P") != 3 {
		t.Fatal("P2P must map to 3")
	}
	if _, err := ParseSpec([]byte("version: v1\n")); err == nil {
		t.Fatal("expected error for wrong version")
	}
}

func TestComputeEdgeCases(t *testing.T) {
	s := NewStore(nil, "", nil)
	tx := Tx{TSMillis: 1_788_000_000_000, AmountMinor: 900, Channel: "ECOMMERCE"}
	none := Raw{DeviceFirst: -1, MCCSeen: -1, PwdChanged: -1, EmailChanged: -1}
	v := s.Compute(none, tx)
	if v[IdxNewDevice] != 1 || v[IdxMCCNovel30d] != 1 || v[IdxMinutesSincePwd] != MinutesCap || v[IdxAmountRatio24h] != 1 {
		t.Fatalf("empty history: %v", v)
	}
	// A credential change at or after as_of never counts (point in time).
	future := Raw{DeviceFirst: tx.TSMillis, MCCSeen: tx.TSMillis + 5, PwdChanged: tx.TSMillis, EmailChanged: tx.TSMillis + 1}
	v = s.Compute(future, tx)
	if v[IdxNewDevice] != 1 || v[IdxMCCNovel30d] != 1 || v[IdxMinutesSincePwd] != MinutesCap || v[IdxMinutesSinceEmail] != MinutesCap {
		t.Fatalf("future sightings must not count: %v", v)
	}
	// A device first seen less than 24 h ago is still new (spec v2); seen before that, it is known.
	if v := s.Compute(Raw{DeviceFirst: tx.TSMillis - Ms24h + 1, MCCSeen: -1, PwdChanged: -1, EmailChanged: -1}, tx); v[IdxNewDevice] != 1 {
		t.Fatalf("first seen 24 h - 1 ms ago must still be new: %v", v)
	}
	recent := Raw{Count24h: 3, Sum24h: 900, DeviceFirst: tx.TSMillis - Ms24h - 1, MCCSeen: tx.TSMillis - Ms30d, PwdChanged: tx.TSMillis - 119_999, EmailChanged: tx.TSMillis - 100*Ms24h}
	v = s.Compute(recent, tx)
	if v[IdxNewDevice] != 0 || v[IdxMCCNovel30d] != 0 || v[IdxMinutesSincePwd] != 1 || v[IdxMinutesSinceEmail] != MinutesCap || v[IdxAmountRatio24h] != 3 {
		t.Fatalf("recent history: %v", v)
	}
	stale := Raw{DeviceFirst: -1, MCCSeen: tx.TSMillis - Ms30d - 1, PwdChanged: -1, EmailChanged: -1}
	v = s.Compute(stale, tx)
	if v[IdxNewDevice] != 1 || v[IdxMCCNovel30d] != 1 {
		t.Fatalf("stale sightings must not count: %v", v)
	}
	if v[IdxHourOfDay] != float64((tx.TSMillis/Ms1h)%24) || v[IdxChannel] != 2 {
		t.Fatalf("hour/channel: %v", v)
	}
}

func TestFallbackVectorUsesSpecDefaults(t *testing.T) {
	spec := DefaultSpec()
	v := FallbackVector(spec, Tx{TSMillis: 0, AmountMinor: 4200, Channel: "P2P"})
	if v[IdxAmountMinor] != 4200 || v[IdxChannel] != 3 || v[IdxTxCount24h] != float64(spec.Fallback["tx_count_24h"]) {
		t.Fatalf("fallback vector %v", v)
	}
	m := v.Map()
	if len(m) != NumFeatures || m["amount_minor"] != 4200 {
		t.Fatalf("map %v", m)
	}
	if f := v.Float32(); len(f) != NumFeatures || f[IdxAmountMinor] != 4200 {
		t.Fatalf("float32 %v", f)
	}
}
