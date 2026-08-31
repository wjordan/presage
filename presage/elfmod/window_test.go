package elfmod

import "testing"

// TestPairCodeWindowsOrdered: the encoder only emits window sets the plan
// format can carry — ascending and non-overlapping in both images. A pair
// whose sections changed relative order must degrade to the subset that
// fits, never produce a plan predictImage rejects (a non-declined Analyse
// error fails the whole encode).
func TestPairCodeWindowsOrdered(t *testing.T) {
	t.Parallel()
	img := func(secs ...namedSection) *image { return &image{Code: secs} }
	sec := func(name string, off, size uint64) namedSection {
		return namedSection{Name: name, section: section{Addr: off, Off: off, Size: size}}
	}
	cases := []struct {
		name     string
		old, new *image
		want     int
	}{
		{"same order", img(sec(".text", 0, 100), sec(".text.cold", 200, 50)),
			img(sec(".text", 10, 100), sec(".text.cold", 300, 50)), 2},
		{"new side flipped", img(sec(".text", 0, 100), sec(".text.cold", 200, 50)),
			img(sec(".text", 300, 100), sec(".text.cold", 10, 50)), 1},
		{"unpaired dropped", img(sec(".text", 0, 100), sec(".bolt.org.text", 200, 50)),
			img(sec(".text", 10, 100)), 1},
		{"none shared", img(sec(".text", 0, 100)), img(sec(".text.cold", 0, 100)), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			windows := pairCodeWindows(tc.old, tc.new)
			if len(windows) != tc.want {
				t.Fatalf("got %d windows %+v, want %d", len(windows), windows, tc.want)
			}
			var oldEnd, newEnd uint64
			for _, w := range windows {
				if w.Old.Off < oldEnd || w.New.Off < newEnd {
					t.Fatalf("windows violate the plan ordering: %+v", windows)
				}
				oldEnd, newEnd = w.Old.Off+w.Old.Size, w.New.Off+w.New.Size
			}
		})
	}
}
