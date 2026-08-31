//go:build unix

package main

import (
	"os"
	"syscall"
)

// readWhole returns the file's bytes, mapped read-only where the platform
// allows it. Apply reads the reference image once per byte and never writes to
// it, so a mapping spares the 291 MB copy os.ReadFile makes and the page cache
// serves it in place; PROT_READ means a stray write is a crash, not a silent
// corruption of someone's input file. The unmap function is nil for the copy.
func readWhole(name string) ([]byte, func(), error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() <= 0 || int64(int(fi.Size())) != fi.Size() {
		b, err := os.ReadFile(name)
		return b, nil, err
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, int(fi.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		b, err := os.ReadFile(name)
		return b, nil, err
	}
	return b, func() { syscall.Munmap(b) }, nil
}
