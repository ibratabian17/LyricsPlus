package version

import (
	"testing"
)

func TestVersionInfo(t *testing.T) {
	s := String()
	if s == "" {
		t.Fatal("expected non-empty version string")
	}
	if Version == "" {
		t.Fatal("expected Version to not be empty")
	}
	if Commit == "" {
		t.Fatal("expected Commit to not be empty")
	}
	if BuildDate == "" {
		t.Fatal("expected BuildDate to not be empty")
	}
}
