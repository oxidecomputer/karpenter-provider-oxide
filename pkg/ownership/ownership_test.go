package ownership

import (
	"errors"
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

func TestVerify(t *testing.T) {
	expected := Marker{
		ClusterUID: "cluster-a", NodeClaimUID: "claim-a", NodeClaimName: "claim-a", Kind: KindInstance,
	}
	tests := []struct {
		name        string
		description string
		wantReason  RejectionReason
	}{
		{name: "exact owner", description: expected.Description("node")},
		{name: "unmarked", description: "node", wantReason: ReasonUnmarked},
		{name: "malformed", description: prefix + " cluster=cluster-a", wantReason: ReasonMalformed},
		{name: "foreign cluster", description: (Marker{ClusterUID: "cluster-b", NodeClaimUID: "claim-a", NodeClaimName: "claim-a", Kind: KindInstance}).Description(""), wantReason: ReasonForeignCluster},
		{name: "different uid", description: (Marker{ClusterUID: "cluster-a", NodeClaimUID: "claim-b", NodeClaimName: "claim-a", Kind: KindInstance}).Description(""), wantReason: ReasonWrongNodeClaim},
		{name: "different name", description: (Marker{ClusterUID: "cluster-a", NodeClaimUID: "claim-a", NodeClaimName: "claim-b", Kind: KindInstance}).Description(""), wantReason: ReasonWrongNodeClaim},
		{name: "wrong kind", description: (Marker{ClusterUID: "cluster-a", NodeClaimUID: "claim-a", NodeClaimName: "claim-a", Kind: KindBootDisk}).Description(""), wantReason: ReasonWrongKind},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.description, expected)
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("Verify() error = %v", err)
				}
				return
			}
			var conflict *ErrOwnershipConflict
			if !errors.As(err, &conflict) || conflict.Reason != tt.wantReason {
				t.Fatalf("Verify() error = %#v, want reason %q", err, tt.wantReason)
			}
		})
	}
}

func TestVerifyClusterResource(t *testing.T) {
	want := Marker{
		ClusterUID: "cluster-a", NodeClaimUID: "claim-a", NodeClaimName: "claim-a", Kind: KindInstance,
	}
	got, err := VerifyClusterResource(want.Description("node"), "cluster-a", KindInstance)
	if err != nil || got != want {
		t.Fatalf("VerifyClusterResource() = %#v, %v, want %#v, nil", got, err, want)
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

func TestPoolMarkerRoundTrip(t *testing.T) {
	want := PoolMarker{
		ClusterUID:   types.UID("cluster-uid"),
		BootDiskUID:  types.UID("disk-uid"),
		NodeClassUID: types.UID("class-uid"),
		ConfigHash:   "abc123",
	}
	got, ok := ParsePool(want.Description())
	if !ok || got != want {
		t.Fatalf("ParsePool() = %#v, %t, want %#v, true", got, ok, want)
	}
}
