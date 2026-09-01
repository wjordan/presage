// Package trace is an env-gated stage timeline for the apply path. It is
// off unless PRESAGE_TIMING is set, and costs one boolean test when off.
package trace

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"
)

// On reports whether the timeline is being recorded.
var On = os.Getenv("PRESAGE_TIMING") != ""

var (
	mu    sync.Mutex
	t0    = time.Now()
	spans []span
)

type span struct {
	name             string
	start, end       time.Duration
	cpuStart, cpuEnd time.Duration
}

// cpu is the process's user+system time, so a stage's CPU column counts
// whatever ran anywhere while it was open.
func cpu() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// Stage opens a span named name; the returned func closes it. When the
// timeline is off it returns a no-op.
func Stage(name string) func() {
	if !On {
		return func() {}
	}
	s := span{name: name, start: time.Since(t0), cpuStart: cpu()}
	return func() {
		s.end, s.cpuEnd = time.Since(t0), cpu()
		mu.Lock()
		spans = append(spans, s)
		mu.Unlock()
	}
}

// Stagef is Stage with a formatted name; the name is built only when the
// timeline is on.
func Stagef(format string, a ...any) func() {
	if !On {
		return func() {}
	}
	return Stage(fmt.Sprintf(format, a...))
}

// Report prints the timeline to stderr, in start order, and clears it.
func Report() {
	if !On {
		return
	}
	mu.Lock()
	got := spans
	spans = nil
	mu.Unlock()
	sort.SliceStable(got, func(i, j int) bool { return got[i].start < got[j].start })
	fmt.Fprintf(os.Stderr, "%-34s %9s %9s %9s %9s\n", "stage", "start", "end", "wall", "cpu")
	for _, s := range got {
		fmt.Fprintf(os.Stderr, "%-34s %9.3f %9.3f %9.3f %9.3f\n", s.name,
			s.start.Seconds(), s.end.Seconds(), (s.end - s.start).Seconds(),
			(s.cpuEnd - s.cpuStart).Seconds())
	}
	fmt.Fprintf(os.Stderr, "%-34s %9s %9.3f %9.3f %9.3f\n", "TOTAL", "", time.Since(t0).Seconds(),
		time.Since(t0).Seconds(), cpu().Seconds())
}
