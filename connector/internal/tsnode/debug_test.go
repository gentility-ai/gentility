package tsnode

import "testing"

func TestNetDebugIsOnlyTheWordOne(t *testing.T) {
	for _, tc := range []struct {
		value string
		set   bool
		want  bool
	}{
		{"1", true, true},
		{"true", true, false},
		{"0", true, false},
		{"", true, false},
		{"", false, false},
	} {
		lookup := func(string) (string, bool) { return tc.value, tc.set }
		if got := netDebugWanted(lookup); got != tc.want {
			t.Fatalf("%q set=%v: got %v", tc.value, tc.set, got)
		}
	}
}
