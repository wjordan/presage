//go:build corpus

// A diagnostic for the function map: how many of one code section's units
// each pass of constructPlan would pair, and how ambiguous the content
// pass is. Manual-run, on a pair named by the environment:
//
//	PROBE_OLD=old.so PROBE_NEW=new.so PROBE_SEC=.bolt.org.text \
//	  go test -tags corpus ./presage/elfmod -run TestMapProbe -v
//
// PROBE_CANON=1 additionally canonicalises names with a regex, the
// measurement stand-in used before symbols.CanonicalName existed; it is
// kept so the before/after of docs/general/research/domain-rust.md §6.1
// can be reproduced.
package elfmod

import (
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/wjordan/presage/delta/x86"
	"github.com/wjordan/presage/presage/symbols"
)

// canonReader erases Rust v0 crate disambiguators from symbol names, the
// measurement stand-in for a real v0 parse.
var v0Disambig = regexp.MustCompile(`Cs[0-9A-Za-z]+_`)

type canonReader struct{ r symbols.Reader }

func (c canonReader) Funcs(visit func(symbols.Func)) error {
	return c.r.Funcs(func(f symbols.Func) {
		if len(f.Name) > 2 && f.Name[0] == '_' && f.Name[1] == 'R' {
			f.Name = v0Disambig.ReplaceAllString(f.Name, "Cs_")
		}
		visit(f)
	})
}

// TestMapProbe reports how the function map would be built for one code
// section of a pair named by PROBE_OLD/PROBE_NEW/PROBE_SEC.
func TestMapProbe(t *testing.T) {
	oldPath, newPath := os.Getenv("PROBE_OLD"), os.Getenv("PROBE_NEW")
	if oldPath == "" {
		t.Skip("PROBE_OLD unset")
	}
	secName := os.Getenv("PROBE_SEC")
	if secName == "" {
		secName = ".text"
	}
	load := func(p string) (*image, []codeUnit, section) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		im, err := loadImage(b)
		if err != nil {
			t.Fatal(err)
		}
		sec, ok := im.Sections[secName]
		if !ok {
			t.Fatalf("no section %s", secName)
		}
		r, err := symbols.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv("PROBE_CANON") != "" {
			r = canonReader{r}
		}
		u, err := codeUnits(r, sec)
		if err != nil {
			t.Fatal(err)
		}
		return im, u, sec
	}
	oi, ou, os_ := load(oldPath)
	ni, nu, ns := load(newPath)
	oldText := oi.Data[os_.Off : os_.Off+os_.Size]
	newText := ni.Data[ns.Off : ns.Off+ns.Size]
	fmt.Printf("%s: %d old units, %d new units\n", secName, len(ou), len(nu))

	byName := map[nameID][]int{}
	for i, u := range ou {
		for _, n := range u.Names {
			byName[n] = append(byName[n], i)
		}
	}
	nameMapped := map[int]bool{}
	for ni2, n := range nu {
		for _, nm := range n.Names {
			if len(byName[nm]) > 0 {
				nameMapped[ni2] = true
				break
			}
		}
	}
	byHash := map[hashKey][]int{}
	for oi2, o := range ou {
		byHash[hashKey{Size: o.Size, Hash: x86.ContentHash(code(oldText, o))}] = append(
			byHash[hashKey{Size: o.Size, Hash: x86.ContentHash(code(oldText, o))}], oi2)
	}
	var contentMapped, ambiguous, firstWrong, unmapped int
	bucket := map[int]int{}
	for ni2, n := range nu {
		if nameMapped[ni2] {
			continue
		}
		newCode := code(newText, n)
		k := hashKey{Size: n.Size, Hash: x86.ContentHash(newCode)}
		var eq []int
		for _, oi2 := range byHash[k] {
			if x86.Equal(code(oldText, ou[oi2]), newCode) {
				eq = append(eq, oi2)
			}
		}
		if len(eq) == 0 {
			unmapped++
			continue
		}
		contentMapped++
		b := len(eq)
		if b > 8 {
			b = 9
		}
		bucket[b]++
		if len(eq) > 1 {
			ambiguous++
			best, bestDist := eq[0], uint64(1)<<63
			for _, oi2 := range eq {
				d := max(ou[oi2].Off, n.Off) - min(ou[oi2].Off, n.Off)
				if d < bestDist {
					best, bestDist = oi2, d
				}
			}
			if best != eq[0] {
				firstWrong++
			}
		}
	}
	fmt.Printf("  name-matchable new units: %d (%.1f%%)\n", len(nameMapped), 100*float64(len(nameMapped))/float64(len(nu)))
	fmt.Printf("  content-mapped:           %d\n", contentMapped)
	fmt.Printf("  of those, ambiguous:      %d (%.1f%% of content-mapped)\n", ambiguous, 100*float64(ambiguous)/float64(max(contentMapped, 1)))
	fmt.Printf("  first candidate is not the nearest: %d (%.1f%% of content-mapped)\n", firstWrong, 100*float64(firstWrong)/float64(max(contentMapped, 1)))
	fmt.Printf("  unmapped new units:       %d\n", unmapped)
	fmt.Printf("  equal-candidate bucket sizes: ")
	for i := 1; i <= 9; i++ {
		if bucket[i] > 0 {
			label := fmt.Sprint(i)
			if i == 9 {
				label = ">8"
			}
			fmt.Printf("%s:%d ", label, bucket[i])
		}
	}
	fmt.Println()
}
