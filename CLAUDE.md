# presage — working rules

## Measurement discipline (agents: read before benchmarking anything)

- **Never run `presage diff` to measure apply time.** Use
  `bench/applybench.sh [chrome|libxul]` — it caches reference patches in
  `~/.cache/presage-bench/` and re-encodes only when the cached patch is
  undecodable (format bump) or `-fresh` is passed (when you need the
  patch *size* under the current encoder).
- **Measure once, at the end.** Implement a change set fully, then time
  it (3 runs, median). Per-lever/per-step incremental timing of a 60 s+
  pipeline is how sessions die; a final number plus a list of skipped
  items beats a slow perfect ladder.
- **Run the full gate suite exactly once per change set**, not per step:
  `go vet ./...`, `go test ./... -count=1`, `-race` where concurrency
  changed, and `go test -tags corpus ./presage/elfmod -run
  'TestPairs|TestSelfPrediction|TestNoSymbols' -timeout 10m` (~80 s).
  There is deliberately no multi-pair round-trip test tier; per-pair
  headline sizes are CLI measurements.
- Encode-time measurement: one `presage diff` per binary, `/usr/bin/time`,
  note machine load. Don't rebuild patches you already have.

## Corpus

Chrome pairs + symbols: `~/.cache/presage-chrome-zucchini/`;
libxul/librustc: `~/.cache/presage-pairs/`. Reference tools (zucchini,
bsdiff artifacts) live alongside. Zucchini apply bar on this machine:
3.84 s (chrome .169→.173).

## Format

Pre-release: no back-compat. Wire changes bump `presage.Version`
(comment in the established style in `presage/container.go`); unknown
mode/codec bytes are refused by name. Decode-side tiering (how much CM,
which streams) is encoder policy, never format.

## Research record

`docs/general/research/pgo-churn.md` (structural/coder levers, dead ends
in §7 — read before proposing; refutations are recorded so nobody pays
twice), `encode-profile.md`, `apply-profile.md`. Measured verdicts land
in these docs in the same commit as the code.
