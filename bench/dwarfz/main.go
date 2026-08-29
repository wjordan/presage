// dwarfz exposes the codec's transparent decompression of SHF_COMPRESSED
// sections (delta.ExpandDebug / delta.PackDebug, SPEC §4.5 (a)) so that the
// harness can measure a pair on the plaintext:
//
//	dwarfz plain <elf> <out>        expand every compressed section in place
//	dwarfz pack <plain> <ref> <out> recompress the sections <ref> compresses
//	dwarfz verify <elf>...          check pack(plain(f)) == f
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/wjordan/go-binsync/delta"
)

func main() {
	args := os.Args[1:]
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "dwarfz:", err)
		os.Exit(1)
	}
	read := func(path string) []byte {
		b, err := os.ReadFile(path)
		if err != nil {
			fail(err)
		}
		return b
	}
	switch {
	case len(args) == 3 && args[0] == "plain":
		b := read(args[1])
		out, err := delta.ExpandDebug(b)
		if err != nil {
			fail(err)
		}
		if err := os.WriteFile(args[2], out, 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("%d -> %d bytes\n", len(b), len(out))
	case len(args) == 4 && args[0] == "pack":
		b := read(args[1])
		out, err := delta.PackDebug(b, read(args[2]))
		if err != nil {
			fail(err)
		}
		if err := os.WriteFile(args[3], out, 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("%d -> %d bytes\n", len(b), len(out))
	case len(args) >= 2 && args[0] == "verify":
		for _, path := range args[1:] {
			b := read(path)
			p, err := delta.ExpandDebug(b)
			if err != nil {
				fail(fmt.Errorf("%s: %w", path, err))
			}
			back, err := delta.PackDebug(p, b)
			if err != nil {
				fail(fmt.Errorf("%s: %w", path, err))
			}
			if !bytes.Equal(back, b) {
				fail(fmt.Errorf("%s: pack(plain(f)) differs from f", path))
			}
			fmt.Printf("%s: %d bytes, plain %d, round trip exact\n", path, len(b), len(p))
		}
	default:
		fail(errors.New("usage: dwarfz plain <elf> <out> | pack <plain> <ref> <out> | verify <elf>..."))
	}
}
