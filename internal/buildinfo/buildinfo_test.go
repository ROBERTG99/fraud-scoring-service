package buildinfo

import "testing"

func TestRevisionNeverEmpty(t *testing.T) {
	if Revision() == "" {
		t.Fatal("Revision() returned an empty string")
	}
	if Version == "" {
		t.Fatal("Version is empty")
	}
}
