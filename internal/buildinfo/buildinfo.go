// Package buildinfo exposes version metadata stamped at link time.
package buildinfo

import "runtime/debug"

// Version is overridden with -ldflags "-X .../internal/buildinfo.Version=v1.2.3".
var Version = "dev"

// Revision returns the VCS revision embedded by the Go toolchain, or "unknown".
func Revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return s.Value
		}
	}
	return "unknown"
}
