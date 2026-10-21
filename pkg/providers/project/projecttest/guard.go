// Package projecttest fails tests that send a parent selector the Oxide API would refuse.
package projecttest

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/oxidecomputer/oxide.go/oxide"
)

// parents are the query parameters that scope a request to a parent resource.
var parents = []string{"project", "vpc", "instance", "router", "gateway", "pool"}

// WithGuard makes a client fail the test on any request that names a resource by ID and still
// sends a parent selector, which Nexus rejects with a 400.
func WithGuard(t *testing.T) oxide.ClientOption {
	t.Helper()
	return oxide.WithHTTPClient(&http.Client{Transport: guard{t: t, next: http.DefaultTransport}})
}

type guard struct {
	t    *testing.T
	next http.RoundTripper
}

func (g guard) RoundTrip(r *http.Request) (*http.Response, error) {
	g.t.Helper()
	if msg := offense(r); msg != "" {
		g.t.Errorf("%s %s %s", r.Method, r.URL.RequestURI(), msg)
	}
	return g.next.RoundTrip(r)
}

// offense explains how a request mixes an ID with a parent selector, or returns "" when it does not.
func offense(r *http.Request) string {
	query := r.URL.Query()
	sent := []string{}
	for _, parent := range parents {
		if query.Has(parent) {
			sent = append(sent, parent)
		}
	}
	// /v1/<resource>/<ref>...: a ref that is an ID forbids every parent selector.
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segments) >= 3 && isID(segments[2]) && len(sent) > 0 {
		return fmt.Sprintf("names a resource by ID but also sent %v", sent)
	}
	// A parent given by ID forbids the parents above it.
	if isID(query.Get("vpc")) && query.Has("project") {
		return "names the VPC by ID but also sent a project"
	}
	for _, parent := range []string{"instance", "router", "gateway", "pool"} {
		if isID(query.Get(parent)) && (query.Has("project") || query.Has("vpc")) {
			return fmt.Sprintf("names the %s by ID but also sent a project or VPC", parent)
		}
	}
	return ""
}

func isID(ref string) bool {
	_, err := uuid.Parse(ref)
	return err == nil
}
