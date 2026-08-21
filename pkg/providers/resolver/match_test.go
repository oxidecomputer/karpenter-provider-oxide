package resolver

import "testing"

func TestMatchVersion(t *testing.T) {
	tests := []struct {
		pattern string
		version string
		want    bool
	}{
		// No wildcard: the pre-wildcard behavior, an exact case-insensitive compare.
		{"1.2.3", "1.2.3", true},
		{"1.2.3", "1.2.30", false},
		{"1.2.3", "v1.2.3", false},
		{"24.04 2026-05-09", "24.04 2026-05-09", true},
		{"NOBLE", "noble", true},
		{"12", "12", true},
		{"12", "13", false},

		// Semver-shaped wildcards.
		{"1.2.*", "1.2.3", true},
		{"1.2.*", "1.2.11", true},
		{"1.2.*", "1.2", false},
		{"1.2.*", "1.3.0", false},
		{"1.*", "1.4", true},
		{"1.*", "1.4.2", true},
		{"1.*", "1", false},
		{"1.*", "10.1", false},
		{"1.*.3", "1.2.3", true},
		{"1.*.3", "1.2.3.4", false},
		{"*.2.3", "1.2.3", true},

		// A wildcard never consumes part of a segment, which is the whole reason segments exist.
		{"1.2.*", "1.20", false},
		{"1.*", "12.0", false},

		// Leading "v" is ignored on either side.
		{"v1.2.*", "1.2.3", true},
		{"1.2.*", "v1.2.3", true},
		{"v1.2.*", "v1.2.3", true},
		// ...but only as a prefix of the whole version. Stripping it does not make the rest of the
		// leading segment match a shorter one.
		{"1.*", "1.v2", true},
		{"v1.*", "v1x.2", false},

		// Date-shaped versions. "-" and " " are both segment separators.
		{"24.04 *", "24.04 2026-05-09", true},
		{"24.04 *", "24.10 2026-05-09", false},
		{"24.04 2026-05-*", "24.04 2026-05-09", true},
		{"24.04 2026-05-*", "24.04 2026-06-09", false},

		// Bare numbers.
		{"*", "12", true},
		{"*", "1.2.3", true},
		{"*", "", false},

		// Degenerate patterns.
		{"", "1.2.3", false},
		{"*.*", "1.2", true},
		{"*.*", "1", false},
	}

	for _, tt := range tests {
		if got := MatchVersion(tt.pattern, tt.version); got != tt.want {
			t.Errorf("MatchVersion(%q, %q) = %v, want %v", tt.pattern, tt.version, got, tt.want)
		}
	}
}

func TestMatchName(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		// No wildcard: an exact case-insensitive compare, the same lookup the API would do.
		{"rke2-ubuntu-1780929039", "rke2-ubuntu-1780929039", true},
		{"rke2-ubuntu", "rke2-ubuntu-1780929039", false},
		{"RKE2-Ubuntu", "rke2-ubuntu", true},

		// Prefix selection, the reason wildcards exist here.
		{"rke2-ubuntu-*", "rke2-ubuntu-1780929039", true},
		{"rke2-*", "rke2-ubuntu-1780929039", true},
		{"rke2-*", "k3s-ubuntu-1780929039", false},
		// Unlike a version, a name wildcard is not anchored to a separator.
		{"ubuntu*", "ubuntu-2404", true},
		{"ubuntu*", "ubuntu", true},
		{"ubunt*", "ubuntu-2404", true},

		// Suffix and infix.
		{"*-1780929039", "rke2-ubuntu-1780929039", true},
		{"*-1780929039", "rke2-ubuntu-1780929040", false},
		{"*ubuntu*", "rke2-ubuntu-1780929039", true},
		{"*debian*", "rke2-ubuntu-1780929039", false},

		// Several wildcards must match in order.
		{"rke2-*-178*", "rke2-ubuntu-1780929039", true},
		{"rke2-*-178*", "rke2-1780929039-ubuntu", false},

		// Degenerate patterns.
		{"*", "rke2-ubuntu-1780929039", true},
		{"**", "rke2-ubuntu-1780929039", true},
		{"", "rke2-ubuntu-1780929039", false},
	}

	for _, tt := range tests {
		if got := MatchName(tt.pattern, tt.name); got != tt.want {
			t.Errorf("MatchName(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

// A selector with no version set never reaches matchVersion, but an empty version string on the
// image side does, and it must not be selected by a wildcard that expects real segments.
func TestMatchVersionEmptyImageVersion(t *testing.T) {
	for _, pattern := range []string{"*", "1.*", "1.2.3"} {
		if MatchVersion(pattern, "") {
			t.Errorf("MatchVersion(%q, \"\") = true, want false", pattern)
		}
	}
	if !MatchVersion("", "") {
		t.Error(`MatchVersion("", "") = false, want true`)
	}
}
