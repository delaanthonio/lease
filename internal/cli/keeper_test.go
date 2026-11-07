package cli

import (
	"os"
	"testing"
)

// TestSelfPathResolvesTheRunningBinary pins the keeper launch path: it
// must name an existing file (the actually running binary), falling back
// to os.Args[0] only when the runtime cannot resolve it.
func TestSelfPathResolvesTheRunningBinary(t *testing.T) {
	self := selfPath()
	if self == "" {
		t.Fatal("selfPath returned an empty path")
	}
	info, err := os.Stat(self)
	if err != nil {
		t.Fatalf("selfPath %q does not exist: %v", self, err)
	}
	if info.IsDir() {
		t.Fatalf("selfPath %q is a directory", self)
	}
}
