package features

// FEATURES_UNAVAILABLE path: when Redis errors or exceeds 8 ms the scorer uses the
// conservative training p90 defaults from features/spec.yaml (fallback_defaults), plus the request
// fields that need no history. The combination table then replaces the model band.

// Fallback returns conservative defaults (fallback_defaults in the feature spec) for a transaction when Redis is down.
func (s *Store) Fallback(tx Tx) Vector {
	return FallbackVector(s.spec, tx)
}

// FallbackVector builds the FEATURES_UNAVAILABLE vector from spec defaults plus request fields.
func FallbackVector(spec *Spec, tx Tx) Vector {
	var v Vector
	for i, name := range expectedNames {
		v[i] = float64(spec.Fallback[name])
	}
	v[IdxAmountMinor] = float64(tx.AmountMinor)
	v[IdxChannel] = spec.ChannelCode(tx.Channel)
	v[IdxHourOfDay] = float64((tx.TSMillis / Ms1h) % 24)
	v[IdxAmountRatio24h] = 1
	return v
}
