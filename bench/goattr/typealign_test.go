package main

import "testing"

func TestAlignMethods(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b []string
		want []mpair
		segs []mseg
	}{
		{"identical", []string{"A", "B"}, []string{"A", "B"},
			[]mpair{{0, 0}, {1, 1}}, []mseg{{0, 0, 2}}},
		{"insert at front", []string{"B", "C"}, []string{"A", "B", "C"},
			[]mpair{{0, 1}, {1, 2}}, []mseg{{0, 1, 2}}},
		{"delete in middle", []string{"A", "B", "C"}, []string{"A", "C"},
			[]mpair{{0, 0}, {2, 1}}, []mseg{{0, 0, 1}, {2, 1, 1}}},
		{"disjoint", []string{"A"}, []string{"B"}, nil, nil},
	} {
		got := alignMethods(tc.a, tc.b)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		}
		segs := segsOf(got)
		if len(segs) != len(tc.segs) {
			t.Fatalf("%s: segs %v, want %v", tc.name, segs, tc.segs)
		}
		for i := range segs {
			if segs[i] != tc.segs[i] {
				t.Fatalf("%s: segs %v, want %v", tc.name, segs, tc.segs)
			}
		}
	}
}
