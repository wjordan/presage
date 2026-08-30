package elfmod

import "testing"

func TestPlanReaderRoundTrip(t *testing.T) {
	var b []byte
	b = appendU(b, 300)
	b = appendS(b, -7)
	b = appendStream(b, appendU(nil, 9))
	b = append(b, 0xab)
	r := &planReader{b: b}
	if v := r.u(); v != 300 {
		t.Fatalf("u = %d", v)
	}
	if v := r.s(); v != -7 {
		t.Fatalf("s = %d", v)
	}
	s := r.stream()
	if v := s.u(); v != 9 || !s.done() {
		t.Fatalf("stream = %d, done %v", v, s.done())
	}
	if v := r.byteAt(); v != 0xab || !r.done() {
		t.Fatalf("byte = %#x, done %v", v, r.done())
	}
	if r.byteAt(); r.err == nil {
		t.Fatal("read past the end succeeded")
	}
}

func TestSectionMap(t *testing.T) {
	m := newSectionMap(map[string]section{
		".text": {Addr: 0x1000, Off: 0x1000, Size: 0x100},
		".data": {Addr: 0x3000, Off: 0x2000, Size: 0x10},
		".bss":  {Addr: 0x4000, Off: 0x2010, Size: 0x100, NoBits: true},
	})
	if len(m) != 2 || m[0].Old != 0x1000 {
		t.Fatalf("map = %+v", m)
	}
	if off, ok := m.offsetOf(0x3008); !ok || off != 0x2008 {
		t.Fatalf("offsetOf = %#x, %v", off, ok)
	}
	if _, ok := m.offsetOf(0x1100); ok {
		t.Fatal("offsetOf past .text succeeded")
	}
	if addr, ok := m.addrOf(0x1010); !ok || addr != 0x1010 {
		t.Fatalf("addrOf = %#x, %v", addr, ok)
	}
	if _, ok := m.addrOf(0x2010); ok {
		t.Fatal("addrOf in the gap succeeded")
	}
	r := &planReader{b: m.marshal(nil)}
	got, err := unmarshalSectionMap(r)
	if err != nil || !r.done() || len(got) != 2 || got[1] != m[1] {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
}
