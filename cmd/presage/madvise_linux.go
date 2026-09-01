//go:build linux

package main

import "syscall"

func canDiscardWhole() bool { return true }

func discardWhole(b []byte) { _ = syscall.Madvise(b, syscall.MADV_DONTNEED) }
