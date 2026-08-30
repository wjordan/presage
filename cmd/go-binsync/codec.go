package main

import (
	"bytes"
	"log/slog"
	"os"
	"time"

	"github.com/wjordan/presage/codec"
	"github.com/wjordan/presage/presage"
	"github.com/wjordan/presage/release"
)

// diff and patch are offline access to the codec, for development and for the
// benchmark harness (README.md 5).
func diff(log *slog.Logger, args []string) error {
	fs := newFlags("diff", "<old> <new> -o <patch>")
	out := fs.String("o", "", "write the patch here (required)")
	verbose := fs.Bool("v", false, "report where the patch bytes went")
	plain := fs.Bool("plain", false, "skip the predictive modules")
	legacy := fs.Bool("legacy", false, "write the delta container for agents that predate presage")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || *out == "" {
		return exitf(codeUsage, "diff needs two binaries and -o")
	}
	old, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	next, err := os.ReadFile(pos[1])
	if err != nil {
		return err
	}

	start := time.Now()
	var st presage.Stats
	patch, err := codec.Encode(old, next, codec.Options{Plain: *plain, Legacy: *legacy, Stats: &st})
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, patch, 0o644); err != nil {
		return err
	}
	log.Info("go-binsync: encoded", "patch", hbytes(int64(len(patch))), "of", hbytes(int64(len(next))),
		"modules", codec.Modules(&st), "took", hdur(time.Since(start)))
	if *verbose {
		for i, r := range st.Regions {
			log.Info("go-binsync: region", "n", i, "module", r.Module, "bytes", hbytes(r.Length),
				"plan", hbytes(int64(r.Plan)), "residual", hbytes(int64(r.Residual)), "mispredicted", hbytes(int64(r.PredictErr)))
		}
		for _, n := range st.Notes {
			log.Info("go-binsync: " + n)
		}
	}
	return nil
}

func patchCmd(log *slog.Logger, args []string) error {
	fs := newFlags("patch", "<old> <patch> -o <new>")
	out := fs.String("o", "", "write the result here (required)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || *out == "" {
		return exitf(codeUsage, "patch needs a binary, a patch and -o")
	}
	old, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	p, err := os.ReadFile(pos[1])
	if err != nil {
		return err
	}

	start := time.Now()
	var buf bytes.Buffer
	buf.Grow(len(old))
	if err := codec.Apply(old, p, &buf); err != nil {
		if codec.Unsupported(err) {
			return err
		}
		// Everything else Apply refuses is the result not being what the
		// patch promised (README.md 5, exit code 3).
		return &exitError{codeVerify, err}
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o755); err != nil {
		return err
	}
	log.Info("go-binsync: applied", "release", release.HashBytes(buf.Bytes()),
		"size", hbytes(int64(buf.Len())), "took", hdur(time.Since(start)))
	return nil
}
