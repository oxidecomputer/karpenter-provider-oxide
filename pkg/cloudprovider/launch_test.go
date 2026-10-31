package cloudprovider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/bootdisk"
	diskprovider "github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/disk"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project/projecttest"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	"github.com/oxidecomputer/oxide.go/oxide"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpcorev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

func TestSelectSubnetRejectsAnEmptyStatus(t *testing.T) {
	_, err := selectSubnet(nil)
	if !karpcloudprovider.IsNodeClassNotReadyError(err) {
		t.Fatalf("selectSubnet() error = %v, want NodeClassNotReadyError", err)
	}
}

func TestExternalIPsIsOneAttachmentPerEntry(t *testing.T) {
	tests := []struct {
		name         string
		ephemeralIPs []v1alpha1.OxideEphemeralIP
	}{
		{
			name: "a nodeclass declaring none asks for none",
		},
		{
			name:         "one entry is one attachment",
			ephemeralIPs: []v1alpha1.OxideEphemeralIP{{IPVersion: "v4", PoolName: "public-v4"}},
		},
		{
			name: "and a dual-stack pair is two",
			ephemeralIPs: []v1alpha1.OxideEphemeralIP{
				{IPVersion: "v4"},
				{IPVersion: "v6", PoolName: "public-v6"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := externalIPSets(tt.ephemeralIPs)[0]; len(got) != len(tt.ephemeralIPs) {
				t.Errorf(
					"externalIPSets() asked for %d attachments, want one per declared entry (%d)",
					len(got),
					len(tt.ephemeralIPs),
				)
			}
		})
	}
}

func TestExternalIPSetsTriesEveryPoolCombinationFirstChoicesFirst(t *testing.T) {
	sets := externalIPSets([]v1alpha1.OxideEphemeralIP{
		{IPVersion: "v4", PoolName: "v4-a", Fallbacks: []v1alpha1.OxideIPPool{{Name: "v4-b"}}},
		{IPVersion: "v6", PoolName: "v6-a", Fallbacks: []v1alpha1.OxideIPPool{{Name: "v6-b"}}},
	})
	got := make([]string, 0, len(sets))
	for _, set := range sets {
		names := make([]string, 0, len(set))
		for _, externalIP := range set {
			selector := externalIP.Value.(*oxide.ExternalIpCreateEphemeral).PoolSelector
			names = append(names, string(selector.Value.(*oxide.PoolSelectorExplicit).Pool))
		}
		got = append(got, strings.Join(names, "+"))
	}
	want := []string{"v4-a+v6-a", "v4-a+v6-b", "v4-b+v6-a", "v4-b+v6-b"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("externalIPSets() = %v, want %v", got, want)
	}
}

func TestExternalIPSetsSelectsThePoolTheEntryNamed(t *testing.T) {
	tests := []struct {
		name        string
		ephemeralIP v1alpha1.OxideEphemeralIP
		want        oxide.PoolSelector
	}{
		{
			name:        "an entry with a pool selects it explicitly",
			ephemeralIP: v1alpha1.OxideEphemeralIP{IPVersion: "v4", PoolName: "public-v4"},
			want: oxide.PoolSelector{
				Value: &oxide.PoolSelectorExplicit{Pool: oxide.NameOrId("public-v4")},
			},
		},
		{
			name:        "an entry with no pool takes the silo default for its version",
			ephemeralIP: v1alpha1.OxideEphemeralIP{IPVersion: "v6"},
			want: oxide.PoolSelector{
				Value: &oxide.PoolSelectorAuto{IpVersion: oxide.IpVersionV6},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := externalIPSets([]v1alpha1.OxideEphemeralIP{tt.ephemeralIP})[0]
			if len(got) != 1 {
				t.Fatalf("externalIPSets() = %v, want one attachment", got)
			}
			ephemeral, ok := got[0].Value.(*oxide.ExternalIpCreateEphemeral)
			if !ok {

				t.Fatalf(
					"externalIPSets() asked for %T, want an ephemeral attachment",
					got[0].Value,
				)
			}
			if ephemeral.PoolSelector.Type() != tt.want.Type() {
				t.Fatalf(
					"externalIPSets() selected the pool by %q, want %q",
					ephemeral.PoolSelector.Type(),
					tt.want.Type(),
				)
			}
			switch want := tt.want.Value.(type) {
			case *oxide.PoolSelectorExplicit:
				got := ephemeral.PoolSelector.Value.(*oxide.PoolSelectorExplicit)
				if got.Pool != want.Pool {
					t.Errorf("externalIPSets() named pool %q, want %q", got.Pool, want.Pool)
				}
			case *oxide.PoolSelectorAuto:
				got := ephemeral.PoolSelector.Value.(*oxide.PoolSelectorAuto)
				if got.IpVersion != want.IpVersion {
					t.Errorf(
						"externalIPSets() asked the default pool for %q, want %q",
						got.IpVersion,
						want.IpVersion,
					)
				}
			}
		})
	}
}

func TestOwnershipMarksEveryResourceOfALaunch(t *testing.T) {
	c := &OxideCloudProvider{clusterUID: types.UID("cluster-uid")}
	nodeClaim := &karpcorev1.NodeClaim{
		Name: "default-abc12", UID: types.UID("nodeclaim-uid"),
	}
	describe := c.ownership(nodeClaim)

	for _, kind := range []ownership.Kind{
		ownership.KindInstance,
		ownership.KindBootDisk,
		ownership.KindDataDisk,
	} {
		human := "diskSet logs disk 1"
		description := describe(kind, human)

		marker, ok := ownership.Parse(description)
		if !ok {
			t.Fatalf("Parse(%q) found no marker", description)
		}
		want := ownership.Marker{
			ClusterUID:    "cluster-uid",
			NodeClaimUID:  "nodeclaim-uid",
			NodeClaimName: "default-abc12",
			Kind:          kind,
		}
		if marker != want {
			t.Errorf("Parse(%q) = %#v, want %#v", description, marker, want)
		}

		if !strings.Contains(description, human) {
			t.Errorf("description %q lost the human-readable text %q", description, human)
		}
	}
}

func TestTeardownUsesTheRecordedDiskPrefix(t *testing.T) {
	nodeClaim := &karpcorev1.NodeClaim{
		Name: "default-abc12",
		Annotations: map[string]string{
			v1alpha1.AnnotationDiskPrefix: "kd-abcdefghijkl-",
			v1alpha1.AnnotationBootDisk:   "pooled-boot",
		},
	}

	owner := ownership.Marker{
		ClusterUID:    "cluster",
		NodeClaimUID:  "claim",
		NodeClaimName: nodeClaim.Name,
		Kind:          ownership.KindInstance,
	}
	teardown := teardownFor(nodeClaim, "karpenter-default-abc12", owner)
	if teardown.DiskPrefix != "kd-abcdefghijkl-" {
		t.Errorf("disk prefix = %q, want the prefix the launch recorded", teardown.DiskPrefix)
	}

	if teardown.BootDiskName != "pooled-boot" {
		t.Errorf("boot disk name = %q, want the recorded pooled disk", teardown.BootDiskName)
	}
	if teardown.Owner != owner {
		t.Errorf("owner = %#v, want %#v", teardown.Owner, owner)
	}
}

func TestTeardownFallsBackToTheLegacyNamesWithoutARecord(t *testing.T) {
	nodeClaim := &karpcorev1.NodeClaim{Name: "default-abc12"}

	owner := ownership.Marker{
		ClusterUID:    "cluster",
		NodeClaimUID:  "claim",
		NodeClaimName: nodeClaim.Name,
		Kind:          ownership.KindInstance,
	}
	teardown := teardownFor(nodeClaim, "karpenter-default-abc12", owner)
	if teardown.BootDiskName != "karpenter-default-abc12" {
		t.Errorf("boot disk name = %q, want the legacy name", teardown.BootDiskName)
	}
	if want := storage.LegacyPrefix("karpenter-default-abc12"); teardown.DiskPrefix != want {
		t.Errorf("disk prefix = %q, want the legacy prefix %q", teardown.DiskPrefix, want)
	}
}

func TestReleaseUnstartedBootDiskRetiresWhenTheDiskIsUnreadable(t *testing.T) {
	record := &v1alpha1.OxideBootDisk{
		Name: "pool-record",
		Spec: v1alpha1.OxideBootDiskSpec{
			DiskName: "pool-disk",
			ClaimRef: &v1alpha1.BootDiskOwnerRef{Name: "claim", UID: types.UID("claim-uid")},
		},
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(record).
		Build()
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {},
	))
	server.Close()
	ox, err := oxide.NewClient(
		oxide.WithHost(server.URL),
		oxide.WithToken("token"),
		projecttest.WithGuard(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	pool := bootdisk.NewProvider(
		kubeClient,
		kubeClient,
		diskprovider.NewProvider(ox, "project"),
		"cluster",
	)
	provider := &OxideCloudProvider{bootDiskPool: pool}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := false
	provider.releaseUnstartedBootDisk(ctx, &bootdisk.Acquired{
		RecordName: record.Name,
		DiskName:   record.Spec.DiskName,
		ClaimUID:   record.Spec.ClaimRef.UID,
	}, &started)

	got := &v1alpha1.OxideBootDisk{}
	if err := kubeClient.Get(
		context.Background(),
		client.ObjectKey{Name: record.Name},
		got,
	); err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Retire {
		t.Fatal("a boot disk of unknown state was not retired after an aborted launch")
	}
}

func TestCreateRejectsAnUnreadyNodeClass(t *testing.T) {
	nodeClass := &v1alpha1.OxideNodeClass{Name: "default"}
	nodeClass.StatusConditions().SetFalse(
		v1alpha1.ConditionTypeSubnetsResolved,
		"SubnetsNotFound",
		"no subnets matched",
	)
	c := &OxideCloudProvider{
		kubeClient: fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(nodeClass).
			WithStatusSubresource(nodeClass).
			Build(),
	}
	nodeClaim := &karpcorev1.NodeClaim{
		Name: "claim",
		Spec: karpcorev1.NodeClaimSpec{
			NodeClassRef: &karpcorev1.NodeClassReference{Name: "default"},
		},
	}

	_, err := c.Create(context.Background(), nodeClaim)
	if !karpcloudprovider.IsNodeClassNotReadyError(err) {
		t.Fatalf("Create() error = %v, want NodeClassNotReadyError", err)
	}
}
