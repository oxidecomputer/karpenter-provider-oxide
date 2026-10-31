package storage

import (
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func sourcedNodeClass(t *testing.T, resolvedImageID string) *v1alpha1.OxideNodeClass {
	t.Helper()
	nc := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Storage: v1alpha1.Storage{
				Boot: v1alpha1.BootDisk{Size: resource.MustParse("20Gi")},
				DiskSets: []v1alpha1.DiskSet{{
					Name: "restored", Type: v1alpha1.DiskSetTypeDistributed, Count: 1,
					Size: resource.MustParse("100Gi"), MountPath: "/var/lib/data",
					ImageSelector: []v1alpha1.ImageSelector{{Name: "golden-*"}},
				}},
			},
		},
	}
	if resolvedImageID != "" {
		nc.Status.Storage = &v1alpha1.StorageStatus{
			DiskSets: []v1alpha1.DiskSetStatus{{
				Name:   "restored",
				Images: []v1alpha1.OxideImage{{ID: resolvedImageID}},
			}},
		}
	}
	return nc
}

func claimWith(t *testing.T, annotations map[string]string) *karpv1.NodeClaim {
	t.Helper()
	return &karpv1.NodeClaim{Annotations: annotations}
}

func TestDriftReason(t *testing.T) {
	tests := []struct {
		name      string
		claim     *karpv1.NodeClaim
		nodeClass *v1alpha1.OxideNodeClass
		want      string
	}{
		{
			name: "the source has not moved",
			claim: claimWith(
				t,
				map[string]string{v1alpha1.AnnotationDiskSetSources: "restored=image-a"},
			),
			nodeClass: sourcedNodeClass(t, "image-a"),
			want:      "",
		},
		{
			name: "a newer image matched the selector",
			claim: claimWith(
				t,
				map[string]string{v1alpha1.AnnotationDiskSetSources: "restored=image-a"},
			),
			nodeClass: sourcedNodeClass(t, "image-b"),
			want:      string(providers.DriftReasonDiskSetSource),
		},
		{

			name:      "no record at all",
			claim:     claimWith(t, nil),
			nodeClass: sourcedNodeClass(t, "image-b"),
			want:      "",
		},
		{

			name: "the source has not resolved",
			claim: claimWith(
				t,
				map[string]string{v1alpha1.AnnotationDiskSetSources: "restored=image-a"},
			),
			nodeClass: sourcedNodeClass(t, ""),
			want:      "",
		},
		{

			name:      "the set was added after the node launched",
			claim:     claimWith(t, map[string]string{v1alpha1.AnnotationDiskSetSources: ""}),
			nodeClass: sourcedNodeClass(t, "image-b"),
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(NewDrifter().DriftReason(tt.claim, tt.nodeClass))
			if got != tt.want {
				t.Errorf("DriftReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBlankDiskSetNeverDriftsOnSource(t *testing.T) {
	nc := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Storage: v1alpha1.Storage{
				Boot: v1alpha1.BootDisk{Size: resource.MustParse("20Gi")},
				DiskSets: []v1alpha1.DiskSet{{
					Name: "scratch", Type: v1alpha1.DiskSetTypeLocal, Count: 2,
					Size: resource.MustParse("100Gi"), MountDir: "/mnt/scratch",
				}},
			},
		},
		Status: v1alpha1.OxideNodeClassStatus{
			Storage: &v1alpha1.StorageStatus{
				DiskSets: []v1alpha1.DiskSetStatus{{Name: "scratch"}},
			},
		},
	}
	claim := claimWith(t, map[string]string{v1alpha1.AnnotationDiskSetSources: "scratch=stale-id"})
	if got := NewDrifter().DriftReason(claim, nc); got != "" {
		t.Errorf("DriftReason() = %q for a blank diskSet, want no drift", got)
	}
}

func TestDiskSetSourceRecordRoundTrip(t *testing.T) {
	sources := map[string]string{"blank": "", "restored": "image-a"}
	encoded := v1alpha1.EncodeDiskSetSources(sources)
	if got, want := encoded, "blank=,restored=image-a"; got != want {
		t.Errorf("EncodeDiskSetSources() = %q, want %q", got, want)
	}
	decoded := v1alpha1.ParseDiskSetSources(encoded)
	if len(decoded) != 2 {
		t.Fatalf("ParseDiskSetSources() returned %d entries, want 2: %v", len(decoded), decoded)
	}
	if value, ok := decoded["blank"]; !ok || value != "" {
		t.Errorf("the empty entry did not survive the round trip: %v", decoded)
	}
	if decoded["restored"] != "image-a" {
		t.Errorf("ParseDiskSetSources()[restored] = %q, want %q", decoded["restored"], "image-a")
	}

	if again := v1alpha1.EncodeDiskSetSources(sources); again != encoded {
		t.Errorf("EncodeDiskSetSources() is not stable: %q then %q", encoded, again)
	}
}
