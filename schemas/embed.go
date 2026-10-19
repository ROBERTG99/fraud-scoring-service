// Package schemas embeds the JSON Schemas shared by Go and Python.
package schemas

import "embed"

// FS holds every schema under schemas/ (events, rules, case summary).
//
//go:embed events/*.json rules.v1.json
var FS embed.FS
