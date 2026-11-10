package instancetype

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestSiloTopologyNormalizesEndpoint(t *testing.T) {
	for _, host := range []string{
		"https://prod.sys.rack-a.example.com",
		"http://PROD.SYS.RACK-A.EXAMPLE.COM.:8080/api/",
		"prod.sys.rack-a.example.com",
		"prod.sys.rack-a.example.com:443/",
	} {
		t.Run(host, func(t *testing.T) {
			got, err := SiloTopology(host, "prod", "silo-id")
			if err != nil {
				t.Fatal(err)
			}
			want := Topology{
				Region: "prod.sys.rack-a.example.com", Zone: "prod.sys.rack-a.example.com",
				SiloHostname: "prod.sys.rack-a.example.com", SiloName: "prod", SiloID: "silo-id",
			}
			if got != want {
				t.Errorf("topology = %#v, want %#v", got, want)
			}
		})
	}
}

func TestSiloTopologyRejectsInvalidEndpoint(t *testing.T) {
	for _, host := range []string{
		"", "https://", "https:///prod", "ftp://prod.example.com", "https://bad_host.example.com",
		"https://prod..example.com", "https://-prod.example.com", "https://prod.example.com:bad",
		"https://[not-an-ip]", "https://prod.example.com..", "https://" + strings.Repeat("a.", 127) + "com",
	} {
		t.Run(host, func(t *testing.T) {
			if got, err := SiloTopology(host, "prod", "silo-id"); err == nil {
				t.Errorf("topology = %#v, want an invalid endpoint error", got)
			}
		})
	}
}

func TestSiloTopologyLabelLength(t *testing.T) {
	for _, tc := range []struct {
		host string
		want string
	}{
		{strings.Repeat("a", 51) + ".example.com", strings.Repeat("a", 51) + ".example.com"},
		{strings.Repeat("a", 63) + ".example.com", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-ab3fee3be0c0eafc796c1cf4f4ce3d45"},
		// Prefix truncation can land on a separator; the result must still be valid.
		{strings.Repeat("a", 29) + "." + strings.Repeat("b", 40) + ".com", ""},
	} {
		got, err := SiloTopology(tc.host, "prod", "silo-id")
		if err != nil {
			t.Fatal(err)
		}
		if problems := validation.IsValidLabelValue(got.Zone); len(problems) != 0 {
			t.Errorf("zone %q: %v", got.Zone, problems)
		}
		if got.Region != got.Zone || got.SiloHostname != tc.host {
			t.Errorf("inconsistent topology: %#v", got)
		}
		if tc.want != "" && got.Zone != tc.want {
			t.Errorf("zone = %q, want %q", got.Zone, tc.want)
		}
	}
}

func TestSiloTopologyDistinguishesEndpoints(t *testing.T) {
	for _, prefix := range []string{"prod.sys.", strings.Repeat("a", 63) + "."} {
		a, err := SiloTopology(prefix+"rack-a.example.com", "prod", "silo-a")
		if err != nil {
			t.Fatal(err)
		}
		b, err := SiloTopology(prefix+"rack-b.example.com", "prod", "silo-b")
		if err != nil {
			t.Fatal(err)
		}
		if a.Zone == b.Zone || a.Region == b.Region {
			t.Errorf("different endpoints share a topology identity: %#v, %#v", a, b)
		}
	}
}

func TestSiloTopologyIPEndpoints(t *testing.T) {
	v4, err := SiloTopology("http://127.0.0.1:8080", "prod", "silo-id")
	if err != nil || v4.Zone != "127.0.0.1" {
		t.Fatalf("IPv4 topology = %#v, error = %v", v4, err)
	}
	var previous Topology
	for _, host := range []string{"http://[2001:db8::1]:8080", "https://[2001:0db8:0:0:0:0:0:1]"} {
		got, err := SiloTopology(host, "prod", "silo-id")
		if err != nil {
			t.Fatal(err)
		}
		if problems := validation.IsValidLabelValue(got.Zone); len(problems) != 0 {
			t.Errorf("zone %q: %v", got.Zone, problems)
		}
		if got.SiloHostname != "2001:db8::1" || got.Region != got.Zone {
			t.Errorf("inconsistent IPv6 topology: %#v", got)
		}
		if previous.Zone != "" && got != previous {
			t.Errorf(
				"equivalent IP endpoints produced different identities: %#v, %#v",
				previous,
				got,
			)
		}
		previous = got
	}
}
