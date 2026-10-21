package projecttest

import (
	"net/http"
	"net/url"
	"testing"
)

func TestGuardRejectsParentsAlongsideAnID(t *testing.T) {
	for _, tt := range []struct {
		url  string
		want bool
	}{
		{url: "/v1/images/11111111-0000-4000-8000-000000000001"},
		{url: "/v1/images/base?project=prod"},
		{url: "/v1/vpc-subnets?vpc=prod&project=prod"},
		{url: "/v1/vpc-subnets?vpc=11111111-0000-4000-8000-000000000001"},
		{url: "/v1/images/11111111-0000-4000-8000-000000000001?project=prod", want: true},
		{url: "/v1/vpc-subnets/11111111-0000-4000-8000-000000000001?vpc=prod", want: true},
		{url: "/v1/vpc-subnets?vpc=11111111-0000-4000-8000-000000000001&project=prod", want: true},
		{url: "/v1/instances/11111111-0000-4000-8000-000000000001/disks?project=prod", want: true},
	} {
		t.Run(tt.url, func(t *testing.T) {
			u, err := url.Parse("http://rack" + tt.url)
			if err != nil {
				t.Fatal(err)
			}
			msg := offense(&http.Request{Method: http.MethodGet, URL: u})
			if got := msg != ""; got != tt.want {
				t.Errorf("offense() = %q, want an offense: %t", msg, tt.want)
			}
		})
	}
}
