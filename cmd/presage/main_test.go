package main

import "testing"

func TestDefaultEncodeMemoryLimit(t *testing.T) {
	const mib = int64(1 << 20)
	for _, tc := range []struct {
		reference, target, want int64
	}{
		{1, 1, 1536 * mib},
		{100 * mib, 200 * mib, 1600 * mib},
		{300 * mib, 100 * mib, 2400 * mib},
	} {
		if got := defaultEncodeMemoryLimit(tc.reference, tc.target); got != tc.want {
			t.Errorf("defaultEncodeMemoryLimit(%d, %d) = %d, want %d", tc.reference, tc.target, got, tc.want)
		}
	}
}

func TestDefaultApplyMemoryLimit(t *testing.T) {
	const mib = int64(1 << 20)
	for _, tc := range []struct {
		target, want int64
	}{
		{1, 256 * mib},
		{100 * mib, 256 * mib},
		{300 * mib, 450 * mib},
	} {
		if got := defaultApplyMemoryLimit(tc.target); got != tc.want {
			t.Errorf("defaultApplyMemoryLimit(%d) = %d, want %d", tc.target, got, tc.want)
		}
	}
}

func TestProfileDestinationsDiffer(t *testing.T) {
	if _, err := startProfiles(&profileFlags{cpu: "same", trace: "same"}); err == nil {
		t.Fatal("duplicate profile destination accepted")
	}
	stop, err := startProfiles(&profileFlags{})
	if err != nil {
		t.Fatal(err)
	}
	stop()
}
