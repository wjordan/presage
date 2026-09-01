//go:build unix && !linux

package main

func canDiscardWhole() bool { return false }

func discardWhole([]byte) {}
