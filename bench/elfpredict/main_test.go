package main

import (
	"os"
	"testing"
)

// TestMain keeps the stage memo out of the shared cache directory.
// unmarshalPlan builds the reference-target domain, so every plan test would
// otherwise leave an entry behind in it and, worse, could read one a previous
// run wrote.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "elfpredict-test-")
	if err != nil {
		panic(err)
	}
	memoDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
