package presage

import (
	"bytes"
	"math/rand"
	"testing"
)

func eqRegistry() *Registry {
	r := NewRegistry()
	r.Add(EqModule{})
	return r
}

// A moved block with scattered edits: eq claims it and round-trips. (It is
// not smaller than lz here or anywhere measured; see ModuleEq.)
func TestEqModuleRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	old := make([]byte, 1<<18)
	r.Read(old)
	new := append(append(append([]byte{}, old[1<<12:1<<17]...), make([]byte, 999)...), old[:1<<12]...)
	for range 200 {
		new[r.Intn(len(new))] ^= 0xff
	}
	var eq, lz Stats
	roundTrip(t, [][]byte{old}, new, Options{Registry: eqRegistry(), Stats: &eq})
	roundTrip(t, [][]byte{old}, new, Options{Stats: &lz})
	if eq.Regions[0].Module != "eq" {
		t.Fatalf("module %s, want eq", eq.Regions[0].Module)
	}
	if eq.Total > 2500 {
		t.Fatalf("eq patch is %d bytes for 200 byte edits and one move", eq.Total)
	}
	t.Logf("eq %d B, lz %d B", eq.Total, lz.Total)
}

func TestEqModuleDeclinesUnrelated(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	old, new := make([]byte, 1<<14), make([]byte, 1<<14)
	r.Read(old)
	r.Read(new)
	var st Stats
	roundTrip(t, [][]byte{old}, new, Options{Registry: eqRegistry(), Stats: &st})
	if st.Regions[0].Module != "lz" {
		t.Fatalf("module %s, want lz for unrelated inputs", st.Regions[0].Module)
	}
}

func TestEqMaterialiseRejectsRunsOutsideTheImages(t *testing.T) {
	old := bytes.Repeat([]byte("abcdefgh"), 1024)
	patch, err := Encode([][]byte{old}, old[:4096], Options{Registry: eqRegistry(), Modules: []byte{ModuleEq, ModuleLZ}})
	if err != nil {
		t.Fatal(err)
	}
	// Shrinking the reference makes every run leave it.
	if err := Apply([][]byte{old[:100]}, patch, eqRegistry(), &bytes.Buffer{}); err == nil {
		t.Fatal("a plan whose runs leave the reference was accepted")
	}
}
