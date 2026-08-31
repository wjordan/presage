#!/usr/bin/env bash
# applybench.sh — time `presage patch` without paying an encode per data point.
#
#   bench/applybench.sh [-fresh] [-n RUNS] [chrome|libxul]
#
# Patches are cached in ~/.cache/presage-bench/ per pair. The default run
# reuses the newest cached patch (apply-side changes never need a fresh
# encode; a format bump makes the old patch fail decode, which triggers a
# re-encode automatically). -fresh forces a re-encode with the current
# binary — needed only when the patch SIZE with the current encoder is the
# number you're after. Prints per-run wall time, the median, and cmp result.
set -euo pipefail

fresh=0 runs=3 pair=chrome
while [[ $# -gt 0 ]]; do case $1 in
  -fresh) fresh=1;; -n) runs=$2; shift;; chrome|libxul) pair=$1;;
  *) echo "usage: $0 [-fresh] [-n RUNS] [chrome|libxul]" >&2; exit 2;;
esac; shift; done

C=$HOME/.cache/presage-chrome-zucchini P=$HOME/.cache/presage-pairs
case $pair in
chrome)
  old=$C/chrome-151.0.7922.169 new=$C/chrome-151.0.7922.173
  syms=$C/symbols-151.0.7922.169/debug-info/chrome.debug,$C/symbols-151.0.7922.173/debug-info/chrome.debug;;
libxul)
  old=$P/libxul-154.0.so new=$P/libxul-154.0.1.so
  syms=$P/libxul-154.0.funcs,$P/libxul-154.0.1.funcs;;
esac

cache=$HOME/.cache/presage-bench; mkdir -p "$cache"
bin=$cache/presage; out=$cache/$pair.out
root=$(cd "$(dirname "$0")/.." && pwd)
go build -o "$bin" "$root/cmd/presage"

patch=$(ls -t "$cache/$pair"-*.presage 2>/dev/null | head -1 || true)
encode() {
  patch=$cache/$pair-$(date +%s).presage
  echo "encoding $pair (one-time, ~60-180s)..." >&2
  "$bin" diff -symbols "$syms" "$old" "$new" -o "$patch"
  ls -t "$cache/$pair"-*.presage | tail -n +4 | xargs -r rm -f
}
[[ $fresh = 1 || -z $patch ]] && encode

# A stale-format patch fails decode; re-encode once and retry.
if ! "$bin" patch "$old" "$patch" -o "$out" >/dev/null 2>&1; then
  echo "cached patch not decodable by current binary; re-encoding" >&2
  encode
fi

echo "patch: $patch ($(stat -c%s "$patch") B)"
times=()
for ((i = 1; i <= runs; i++)); do
  t=$({ /usr/bin/time -f%e "$bin" patch "$old" "$patch" -o "$out" >/dev/null; } 2>&1 | tail -1)
  cmp -s "$new" "$out" || { echo "FAIL: output differs from target" >&2; exit 1; }
  echo "run $i: ${t}s"
  times+=("$t")
done
median=$(printf '%s\n' "${times[@]}" | sort -n | awk '{a[NR]=$1} END{print a[int((NR+1)/2)]}')
echo "median apply: ${median}s (cmp OK, $runs runs)"
