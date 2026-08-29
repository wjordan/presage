package codec

import (
	"bytes"
	"testing"
)

func pair() (old, new []byte) {
	old = make([]byte, 40000)
	for i := range old {
		old[i] = byte(i*7 + i/251)
	}
	new = append(append([]byte{}, old[:20000]...), append([]byte("inserted"), old[20000:]...)...)
	return old, new
}

func apply(t *testing.T, old, patch, want []byte) {
	t.Helper()
	var out bytes.Buffer
	if err := Apply(old, patch, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatal("Apply did not reproduce the target")
	}
}

func TestBothContainersApply(t *testing.T) {
	old, new := pair()
	for _, o := range []Options{{}, {Plain: true}, {Legacy: true}} {
		patch, err := Encode(old, new, o)
		if err != nil {
			t.Fatal(err)
		}
		if len(patch) > 200 {
			t.Errorf("%+v: %d-byte patch for one insertion", o, len(patch))
		}
		apply(t, old, patch, new)
	}
}

func TestUnknownFormatIsUnsupported(t *testing.T) {
	old, new := pair()
	patch, err := Encode(old, new, Options{})
	if err != nil {
		t.Fatal(err)
	}
	copy(patch, "ZZZ9")
	err = Apply(old, patch, &bytes.Buffer{})
	if !Unsupported(err) {
		t.Fatalf("unknown magic: %v, want unsupported", err)
	}
	patch, _ = Encode(old, new, Options{})
	patch[4] = 99
	if err := Apply(old, patch, &bytes.Buffer{}); !Unsupported(err) {
		t.Fatalf("future version: %v, want unsupported", err)
	}
	if Unsupported(Apply(old, patch[:3], &bytes.Buffer{})) {
		t.Fatal("a truncated patch is corrupt, not unsupported")
	}
}

func TestWrongOldIsNotUnsupported(t *testing.T) {
	old, new := pair()
	patch, err := Encode(old, new, Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = Apply(new, patch, &bytes.Buffer{})
	if err == nil || Unsupported(err) {
		t.Fatalf("wrong reference: %v", err)
	}
}
