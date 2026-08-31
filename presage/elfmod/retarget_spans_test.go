package elfmod

import "testing"

// TestWindowSpansCoverWholeWindow: the retarget pass visits every byte of a
// code window exactly once, whatever the map looks like. Retargeting only
// the mapped bodies left 39% of a BOLT'd image's code carrying the old
// image's displacements.
func TestWindowSpansCoverWholeWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		maps []mapping
		size uint64
	}{
		{"no maps", nil, 100},
		{"one body, gaps both sides", []mapping{{Dst: 10, DstSize: 20}}, 100},
		{"body at offset zero", []mapping{{Dst: 0, DstSize: 30}}, 100},
		{"body to the end", []mapping{{Dst: 60, DstSize: 40}}, 100},
		{"adjacent bodies", []mapping{{Dst: 0, DstSize: 10}, {Dst: 10, DstSize: 10}}, 100},
		{"whole window mapped", []mapping{{Dst: 0, DstSize: 100}}, 100},
		{"body runs past the end", []mapping{{Dst: 90, DstSize: 50}}, 100},
		{"body starts past the end", []mapping{{Dst: 200, DstSize: 10}}, 100},
		{"overlapping maps", []mapping{{Dst: 0, DstSize: 50}, {Dst: 20, DstSize: 10}}, 100},
		{"zero-sized map", []mapping{{Dst: 10, DstSize: 0}, {Dst: 20, DstSize: 10}}, 100},
		{"empty window", []mapping{{Dst: 0, DstSize: 10}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spans := windowSpans(tc.maps, tc.size)
			var pos uint64
			for _, s := range spans {
				if s.Off != pos {
					t.Fatalf("span at %d, expected %d (spans %+v)", s.Off, pos, spans)
				}
				if s.Size == 0 {
					t.Fatalf("empty span at %d", s.Off)
				}
				pos = s.Off + s.Size
			}
			if pos != tc.size {
				t.Fatalf("spans cover %d of %d bytes (%+v)", pos, tc.size, spans)
			}
		})
	}
}

// TestWindowSpansKeepBodyAlignment: a mapped body is its own span, so the
// instruction decoder still starts at the function's first byte.
func TestWindowSpansKeepBodyAlignment(t *testing.T) {
	t.Parallel()
	maps := []mapping{{Dst: 16, DstSize: 32}, {Dst: 64, DstSize: 16}}
	spans := windowSpans(maps, 96)
	want := []span{{0, 16}, {16, 32}, {48, 16}, {64, 16}, {80, 16}}
	if len(spans) != len(want) {
		t.Fatalf("got %+v, want %+v", spans, want)
	}
	for i := range want {
		if spans[i] != want[i] {
			t.Fatalf("span %d: got %+v, want %+v", i, spans[i], want[i])
		}
	}
}
