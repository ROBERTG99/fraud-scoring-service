// Package featurespec embeds features/spec.yaml for Go consumers.
package featurespec

import _ "embed"

// SpecYAML is features/spec.yaml.
//
//go:embed spec.yaml
var SpecYAML []byte
