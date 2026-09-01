//go:build unix

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscardWholePreservesMapping(t *testing.T) {
	want := bytes.Repeat([]byte("mapped reference"), 4096)
	name := filepath.Join(t.TempDir(), "reference")
	if err := os.WriteFile(name, want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, unmap, err := readWhole(name)
	if err != nil {
		t.Fatal(err)
	}
	if unmap == nil {
		t.Skip("file mapping unavailable")
	}
	defer unmap()
	discardWhole(got)
	if !bytes.Equal(got, want) {
		t.Fatal("discarding clean pages changed the mapping")
	}
}
