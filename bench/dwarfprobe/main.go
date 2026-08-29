// dwarfprobe: does Go's compress/zlib reproduce the linker's compressed
// DWARF sections byte for byte, and at which level?
package main

import (
	"bytes"
	"compress/zlib"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

func main() {
	f, err := elf.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	raw, _ := os.ReadFile(os.Args[1])
	for _, s := range f.Sections {
		if s.Flags&elf.SHF_COMPRESSED == 0 {
			continue
		}
		data := raw[s.Offset : s.Offset+s.FileSize]
		var ch elf.Chdr64
		binary.Read(bytes.NewReader(data), binary.LittleEndian, &ch)
		zr, err := zlib.NewReader(bytes.NewReader(data[24:]))
		if err != nil {
			panic(err)
		}
		plain, _ := io.ReadAll(zr)
		line := fmt.Sprintf("%-18s comp %8d raw %8d:", s.Name, len(data)-24, len(plain))
		for lvl := -1; lvl <= 9; lvl++ {
			var buf bytes.Buffer
			w, _ := zlib.NewWriterLevel(&buf, lvl)
			w.Write(plain)
			w.Close()
			if bytes.Equal(buf.Bytes(), data[24:]) {
				line += fmt.Sprintf(" level %d MATCH", lvl)
			}
		}
		fmt.Println(line)
	}
}
