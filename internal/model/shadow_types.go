package model

// ShadowJob is one decision to re-score with the shadow model.
type ShadowJob struct {
	DecisionID     string
	Features       []float32
	PrimaryScore   float32
	PrimaryVersion string
	PrimaryOK      bool
}
