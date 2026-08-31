package symbols

import "testing"

func TestCanonicalName(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		// not v0: untouched.
		{"main", "main"},
		{"_ZN7rsprobe4main17h042bb855806e1d91E", "_ZN7rsprobe4main17h042bb855806e1d91E"},
		{"_ZN5alloc3vec3Vec4push17hbac808d231589682E", "_ZN5alloc3vec3Vec4push17hbac808d231589682E"},
		{"", ""},
		{"_R", "_R"},
		// v0: every disambiguated crate root loses its disambiguator.
		{
			"_RNvCs3IwsiA6RWiX_8smallvec10deallocate",
			"_RNvCs_8smallvec10deallocate",
		},
		{
			"_RINvCs3IwsiA6RWiX_8smallvec10deallocateNtNtCs71XozMx5lIs_9rustc_hir3hir10GenericArgECsgjmjl2lkvc2_18rustc_ast_lowering",
			"_RINvCs_8smallvec10deallocateNtNtCs_9rustc_hir3hir10GenericArgECs_18rustc_ast_lowering",
		},
		// already canonical: unchanged, and idempotent.
		{"_RNvCs_8smallvec10deallocate", "_RNvCs_8smallvec10deallocate"},
		// `Cs` not followed by <base62>_<digit> is not a crate root.
		{"_RNvC4Cszz3foo", "_RNvC4Cszz3foo"},
	}
	for _, c := range cases {
		if got := CanonicalName(c.in); got != c.want {
			t.Errorf("CanonicalName(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := CanonicalName(CanonicalName(c.in)); got != c.want {
			t.Errorf("CanonicalName not idempotent on %q: %q", c.in, got)
		}
	}
}
