package resolver

import "testing"

func TestMatchVersion(t *testing.T) {
	tests := []struct {
		pattern string
		version string
		want    bool
	}{

		{"1.2.3", "1.2.3", true},
		{"1.2.3", "1.2.30", false},
		{"1.2.3", "v1.2.3", false},
		{"24.04 2026-05-09", "24.04 2026-05-09", true},
		{"NOBLE", "noble", true},
		{"12", "12", true},
		{"12", "13", false},

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

		{"1.2.*", "1.20", false},
		{"1.*", "12.0", false},

		{"v1.2.*", "1.2.3", true},
		{"1.2.*", "v1.2.3", true},
		{"v1.2.*", "v1.2.3", true},

		{"1.*", "1.v2", true},
		{"v1.*", "v1x.2", false},

		{"24.04 *", "24.04 2026-05-09", true},
		{"24.04 *", "24.10 2026-05-09", false},
		{"24.04 2026-05-*", "24.04 2026-05-09", true},
		{"24.04 2026-05-*", "24.04 2026-06-09", false},

		{"*", "12", true},
		{"*", "1.2.3", true},
		{"*", "", false},

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

		{"rke2-ubuntu-1780929039", "rke2-ubuntu-1780929039", true},
		{"rke2-ubuntu", "rke2-ubuntu-1780929039", false},
		{"RKE2-Ubuntu", "rke2-ubuntu", true},

		{"rke2-ubuntu-*", "rke2-ubuntu-1780929039", true},
		{"rke2-*", "rke2-ubuntu-1780929039", true},
		{"rke2-*", "k3s-ubuntu-1780929039", false},

		{"ubuntu*", "ubuntu-2404", true},
		{"ubuntu*", "ubuntu", true},
		{"ubunt*", "ubuntu-2404", true},

		{"*-1780929039", "rke2-ubuntu-1780929039", true},
		{"*-1780929039", "rke2-ubuntu-1780929040", false},
		{"*ubuntu*", "rke2-ubuntu-1780929039", true},
		{"*debian*", "rke2-ubuntu-1780929039", false},

		{"rke2-*-178*", "rke2-ubuntu-1780929039", true},
		{"rke2-*-178*", "rke2-1780929039-ubuntu", false},

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
