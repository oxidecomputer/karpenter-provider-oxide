package ownership

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestMarkerRoundTrip(t *testing.T) {
	want := Marker{
		ClusterUID:    types.UID("cluster-uid"),
		NodeClaimUID:  types.UID("nodeclaim-uid"),
		NodeClaimName: "default-abc12",
		Kind:          KindBootDisk,
	}
	description := want.Description("Boot disk")
	got, ok := Parse(description)
	if !ok {
		t.Fatalf("Parse(%q) did not find the marker", description)
	}
	if got != want {
		t.Fatalf("Parse(%q) = %#v, want %#v", description, got, want)
	}
}

func TestParseRejectsAnythingTheReaperCannotProveItOwns(t *testing.T) {
	for _, description := range []string{
		"Boot disk",
		"karpenter.oxide.computer/owner/v2 cluster=c nodeclaim=n name=nc kind=boot-disk",
		"karpenter.oxide.computer/owner/v1 cluster= nodeclaim=n name=nc kind=boot-disk",
		"karpenter.oxide.computer/owner/v1 cluster=c nodeclaim= name=nc kind=boot-disk",
		"karpenter.oxide.computer/owner/v1 cluster=c nodeclaim=n name= kind=boot-disk",
		"karpenter.oxide.computer/owner/v1 cluster=c nodeclaim=n kind=boot-disk",
		"karpenter.oxide.computer/owner/v1 cluster=c nodeclaim=n name=nc kind=unknown",
	} {
		if got, ok := Parse(description); ok {
			t.Errorf("Parse(%q) = %#v, true; want false", description, got)
		}
	}
}
