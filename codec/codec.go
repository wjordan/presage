// Package codec is the seam between the release lifecycle (publish, agent,
// selfupdate) and the patcher. Patches are presage containers; the delta
// container that earlier releases published is still applied, so a chain
// that straddles the switch still walks.
package codec

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/wjordan/presage/delta"
	"github.com/wjordan/presage/presage"
	"github.com/wjordan/presage/presage/gomod"
)

// Options selects how a patch is encoded.
type Options struct {
	// Plain skips every predictive module: the patch is the lz fallback.
	Plain bool
	// Legacy writes the delta container for a fleet whose agents predate
	// presage; those report a presage patch as a verification failure
	// rather than fall back to the blob.
	Legacy bool
	Stats  *presage.Stats
}

// Encode writes the patch that turns old into new.
func Encode(old, new []byte, o Options) ([]byte, error) {
	if o.Legacy {
		return delta.Encode(old, new, delta.Options{PlainOnly: o.Plain})
	}
	po := presage.Options{Registry: gomod.Registry(), Stats: o.Stats}
	if o.Plain {
		po.Modules = []byte{presage.ModuleLZ}
	}
	return presage.Encode([][]byte{old}, new, po)
}

// Apply writes the file the patch describes, or fails: with an error
// Unsupported reports true when this build cannot read the patch, and with
// any other when the result is not what the patch promised.
func Apply(old, patch []byte, w io.Writer) error {
	switch {
	case bytes.HasPrefix(patch, []byte(presage.Magic)):
		return presage.Apply([][]byte{old}, patch, gomod.Registry(), w)
	case bytes.HasPrefix(patch, []byte(delta.Magic)):
		return delta.Apply(old, patch, w)
	case len(patch) >= 4:
		return &presage.ErrUnsupported{What: fmt.Sprintf("patch format %q", patch[:4])}
	}
	return fmt.Errorf("%w: %d-byte patch", presage.ErrCorrupt, len(patch))
}

// Unsupported reports whether err means the patch needs a codec this build
// lacks, which the caller answers by fetching the blob.
func Unsupported(err error) bool {
	var p *presage.ErrUnsupported
	var d *delta.ErrUnsupportedTransform
	return errors.As(err, &p) || errors.As(err, &d)
}

// Modules names the modules the regions of an encoded patch used, for logs.
func Modules(st *presage.Stats) string {
	var b []byte
	for i, r := range st.Regions {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, r.Module...)
	}
	if len(b) == 0 {
		return "legacy"
	}
	return string(b)
}
