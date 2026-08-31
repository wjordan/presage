//go:build !unix

package main

import "os"

func readWhole(name string) ([]byte, func(), error) {
	b, err := os.ReadFile(name)
	return b, nil, err
}
