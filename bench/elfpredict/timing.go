package main

import (
	"fmt"
	"os"
	"time"
)

// Per-stage timing. Construction is a pipeline of a dozen stages with wildly
// different costs -- two DWARF symbol parses, a 225 MB instruction walk, a few
// hundred xz processes -- and until this printed, the only way to tell which
// one a 25-minute run was sitting in was to watch mtimes in the artifact
// directory. One line per stage on stderr, with the xz share broken out,
// because xz is the cost the -rungs default exists to remove.

var runStart = time.Now()

type stageTimer struct {
	name    string
	t0      time.Time
	xzCalls int64
	xzBytes int64
	xzNanos int64
}

func startStage(name string) *stageTimer {
	return &stageTimer{
		name:    name,
		t0:      time.Now(),
		xzCalls: xzCalls.Load(),
		xzBytes: xzBytes.Load(),
		xzNanos: xzNanos.Load(),
	}
}

// done prints the stage's line and returns its duration. detail says whatever
// makes the stage's size legible -- a symbol count, a byte count -- and may be
// empty.
func (s *stageTimer) done(detail string, args ...any) time.Duration {
	d := time.Since(s.t0)
	line := fmt.Sprintf("stage %-30s %8.2fs  t+%-7.0fs", s.name, d.Seconds(), time.Since(runStart).Seconds())
	if n := xzCalls.Load() - s.xzCalls; n > 0 {
		line += fmt.Sprintf(" [xz %d calls, %.1f MB in, %.0fs cpu]",
			n, float64(xzBytes.Load()-s.xzBytes)/(1<<20),
			time.Duration(xzNanos.Load()-s.xzNanos).Seconds())
	}
	if detail != "" {
		line += "  " + fmt.Sprintf(detail, args...)
	}
	fmt.Fprintln(os.Stderr, line)
	return d
}

// reportTotals closes the run with the one number that says whether the fast
// path is still fast, and the xz share of it.
func reportTotals() {
	fmt.Fprintf(os.Stderr, "stage %-30s %8.2fs  t+%-7.0fs [xz %d calls, %.1f MB in, %.0fs cpu]\n",
		"TOTAL", time.Since(runStart).Seconds(), time.Since(runStart).Seconds(),
		xzCalls.Load(), float64(xzBytes.Load())/(1<<20), time.Duration(xzNanos.Load()).Seconds())
}
