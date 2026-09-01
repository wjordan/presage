package main

import "testing"

func TestDefaultApplyMemoryLimit(t *testing.T) {
	const mib = int64(1 << 20)
	for _, tc := range []struct {
		target, want int64
	}{
		{1, 256 * mib},
		{100 * mib, 256 * mib},
		{300 * mib, 660 * mib},
	} {
		if got := defaultApplyMemoryLimit(tc.target); got != tc.want {
			t.Errorf("defaultApplyMemoryLimit(%d) = %d, want %d", tc.target, got, tc.want)
		}
	}
}
