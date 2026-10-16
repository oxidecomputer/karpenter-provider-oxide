package cloudprovider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/affinitygroup"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/image"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/ippool"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/sshkey"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/subnet"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

const (
	imageID   = "33333333-0000-4000-8000-000000000001"
	imageIDB  = "33333333-0000-4000-8000-000000000002"
	subnetID  = "44444444-0000-4000-8000-000000000001"
	subnetIDB = "44444444-0000-4000-8000-000000000002"
	sshKeyID  = "55555555-0000-4000-8000-000000000001"
	sshKeyIDB = "55555555-0000-4000-8000-000000000002"
	ipPoolID  = "66666666-0000-4000-8000-000000000001"
	ipPoolIDB = "66666666-0000-4000-8000-000000000002"
	groupID   = "77777777-0000-4000-8000-000000000001"
	groupIDB  = "77777777-0000-4000-8000-000000000002"
)

func testDrifters() []providers.Drifter {
	return []providers.Drifter{
		image.NewProvider(nil, 0),
		subnet.NewProvider(nil, 0),
		sshkey.NewProvider(nil, 0),
		ippool.NewProvider(nil, 0),
		affinitygroup.NewProvider(nil, "", 0),
	}
}

func launched(hash string) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
			Annotations: map[string]string{
				v1alpha1.AnnotationOxideNodeClassHash:        hash,
				v1alpha1.AnnotationOxideNodeClassHashVersion: v1alpha1.OxideNodeClassHashVersion,
				v1alpha1.AnnotationSubnet:                    subnetID,
				v1alpha1.AnnotationSSHKeys:                   sshKeyID,
				v1alpha1.AnnotationEphemeralIPs:              "v4=192.0.2.10@" + ipPoolID,
				v1alpha1.AnnotationAffinityGroups:            "aa:" + groupID,
			},
		},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: "karpenter.oxide.computer",
				Kind:  "OxideNodeClass",
				Name:  "default",
			},
		},
		Status: karpv1.NodeClaimStatus{
			ProviderID: "oxide://11111111-0000-4000-8000-000000000001",
			ImageID:    imageID,
		},
	}
}

func resolved(hash string) *v1alpha1.OxideNodeClass {
	return &v1alpha1.OxideNodeClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
			Annotations: map[string]string{
				v1alpha1.AnnotationOxideNodeClassHash:        hash,
				v1alpha1.AnnotationOxideNodeClassHashVersion: v1alpha1.OxideNodeClassHashVersion,
			},
		},
		Status: v1alpha1.OxideNodeClassStatus{
			Images:  []v1alpha1.OxideImage{{ID: imageID}},
			Subnets: []v1alpha1.OxideSubnet{{ID: subnetID}},
			SSHKeys: []v1alpha1.OxideSSHKey{{ID: sshKeyID}},
			EphemeralIPs: []v1alpha1.OxideEphemeralIP{
				{IPVersion: "v4", PoolID: ipPoolID},
			},
			AffinityGroups: []v1alpha1.OxideAffinityGroup{
				{ID: groupID, Type: v1alpha1.AffinityGroupTypeAntiAffinity},
			},
		},
	}
}

func TestIsDrifted(t *testing.T) {
	tests := []struct {
		name      string
		nodeClaim func(*karpv1.NodeClaim)
		nodeClass func(*v1alpha1.OxideNodeClass)
		want      cloudprovider.DriftReason
	}{
		{
			name: "a node launched from the current nodeclass has not drifted",
		},
		{
			name: "a spec edit moves the nodeclass hash, which is static drift",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Annotations[v1alpha1.AnnotationOxideNodeClassHash] = "changed"
			},
			want: providers.DriftReasonNodeClass,
		},
		{
			name: "a nodeclass that has not been stamped yet cannot be compared",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				delete(nodeClass.Annotations, v1alpha1.AnnotationOxideNodeClassHash)
			},
		},
		{
			name: "nor can a NodeClaim launched before the hash was recorded",
			nodeClaim: func(nodeClaim *karpv1.NodeClaim) {
				delete(nodeClaim.Annotations, v1alpha1.AnnotationOxideNodeClassHash)
			},
		},
		{
			name: "a hash recorded at another version is not comparable either",
			nodeClaim: func(nodeClaim *karpv1.NodeClaim) {
				nodeClaim.Annotations[v1alpha1.AnnotationOxideNodeClassHash] = "changed"
				nodeClaim.Annotations[v1alpha1.AnnotationOxideNodeClassHashVersion] = "v-old"
			},
		},
		{
			name: "a newer image winning the selector rolls the nodes onto it",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.Images = []v1alpha1.OxideImage{{ID: imageIDB}, {ID: imageID}}
			},
			want: providers.DriftReasonImage,
		},
		{
			name: "a runner-up image the node is not running is not drift",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.Images = []v1alpha1.OxideImage{{ID: imageID}, {ID: imageIDB}}
			},
		},
		{
			name: "resolving no images at all is a failed resolution, not a fleet replacement",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.Images = nil
			},
		},
		{
			name: "the subnet the interface sits in leaving the selected set is drift",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.Subnets = []v1alpha1.OxideSubnet{{ID: subnetIDB}}
			},
			want: providers.DriftReasonSubnet,
		},
		{
			name: "a subnet joining the selected set is not, since any of them is a valid place to be",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.Subnets = []v1alpha1.OxideSubnet{
					{ID: subnetID},
					{ID: subnetIDB},
				}
			},
		},
		{
			name: "resolving no subnets is a failed resolution, not a fleet replacement",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.Subnets = nil
			},
		},
		{
			name: "a node launched before the subnet was recorded cannot be compared",
			nodeClaim: func(nodeClaim *karpv1.NodeClaim) {
				delete(nodeClaim.Annotations, v1alpha1.AnnotationSubnet)
			},
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.Subnets = []v1alpha1.OxideSubnet{{ID: subnetIDB}}
			},
		},
		{
			name: "an SSH key added has to reach the node, which only a new node can",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.SSHKeys = []v1alpha1.OxideSSHKey{
					{ID: sshKeyID},
					{ID: sshKeyIDB},
				}
			},
			want: providers.DriftReasonSSHKey,
		},
		{
			name: "and an SSH key removed has to stop reaching it",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.SSHKeys = nil
			},
			want: providers.DriftReasonSSHKey,
		},
		{
			name: "a node launched with no keys is not the same as one with no record of them",
			nodeClaim: func(nodeClaim *karpv1.NodeClaim) {
				nodeClaim.Annotations[v1alpha1.AnnotationSSHKeys] = ""
			},
			want: providers.DriftReasonSSHKey,
		},
		{
			name: "a node with no record of its keys cannot be compared",
			nodeClaim: func(nodeClaim *karpv1.NodeClaim) {
				delete(nodeClaim.Annotations, v1alpha1.AnnotationSSHKeys)
			},
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.SSHKeys = nil
			},
		},
		{
			name: "an ephemeral IP entry that no longer matches what was attached is drift",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.EphemeralIPs = []v1alpha1.OxideEphemeralIP{
					{IPVersion: "v4", PoolID: ipPoolIDB},
				}
			},
			want: providers.DriftReasonEphemeralIP,
		},
		{
			name: "a group added to the desired set can only be joined by a new node",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.AffinityGroups = append(
					nodeClass.Status.AffinityGroups,
					v1alpha1.OxideAffinityGroup{
						ID:   groupIDB,
						Type: v1alpha1.AffinityGroupTypeAntiAffinity,
					},
				)
			},
			want: providers.DriftReasonAffinityGroup,
		},
		{
			name: "a group removed from it is left in place instead",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.AffinityGroups = nil
			},
		},
		{
			name: "static drift is reported ahead of the resolved fields it also perturbed",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Annotations[v1alpha1.AnnotationOxideNodeClassHash] = "changed"
				nodeClass.Status.Images = []v1alpha1.OxideImage{{ID: imageIDB}}
			},
			want: providers.DriftReasonNodeClass,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClaim, nodeClass := launched("hash-1"), resolved("hash-1")
			if tt.nodeClaim != nil {
				tt.nodeClaim(nodeClaim)
			}
			if tt.nodeClass != nil {
				tt.nodeClass(nodeClass)
			}

			c := &OxideCloudProvider{
				kubeClient: fake.NewClientBuilder().
					WithScheme(scheme.Scheme).
					WithObjects(nodeClass).
					Build(),
				drifters: testDrifters(),
			}

			got, err := c.IsDrifted(context.Background(), nodeClaim)
			if err != nil {
				t.Fatalf("IsDrifted() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("IsDrifted() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsDriftedWithoutANodeClass(t *testing.T) {
	c := &OxideCloudProvider{
		kubeClient: fake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
		drifters:   testDrifters(),
	}

	got, err := c.IsDrifted(context.Background(), launched("hash-1"))
	if !cloudprovider.IsNodeClassNotReadyError(err) {
		t.Fatalf("IsDrifted() error = %v, want NodeClassNotReadyError", err)
	}
	if got != "" {
		t.Errorf("IsDrifted() = %q, want no drift", got)
	}
}

func TestHashIgnoresSelectors(t *testing.T) {
	nodeClass := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			UserData:      "#!/bin/sh\n",
			ImageSelector: []v1alpha1.ImageSelector{{Name: "ubuntu-*"}},
		},
	}
	before := nodeClass.Hash()

	nodeClass.Spec.ImageSelector = []v1alpha1.ImageSelector{{ID: imageID}}
	nodeClass.Spec.SubnetSelector = []v1alpha1.SubnetSelector{{ID: subnetID}}
	nodeClass.Spec.SSHKeySelector = []v1alpha1.SSHKeySelector{{ID: sshKeyID}}
	nodeClass.Spec.EphemeralIPs = []v1alpha1.EphemeralIP{{Pool: &v1alpha1.IPPoolRef{ID: ipPoolID}}}
	if after := nodeClass.Hash(); after != before {
		t.Errorf("Hash() = %q after rewriting selectors, want the unchanged %q", after, before)
	}

	nodeClass.Spec.UserData = "#!/bin/sh\necho hello\n"
	if after := nodeClass.Hash(); after == before {
		t.Errorf("Hash() = %q after changing user data, want it to differ", after)
	}
}

func TestHashCoversLaunchTimeSpecFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1alpha1.OxideNodeClassSpec)
	}{
		{
			name:   "user data",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) { spec.UserData = "#!/bin/sh\n" },
		},
		{
			name: "environment",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.Env = []v1alpha1.EnvVar{{Name: "HTTP_PROXY", Value: "http://proxy:3128"}}
			},
		},
		{
			name:   "the join server",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) { spec.Server = "10.0.0.1:6443" },
		},
		{
			name: "the kubelet pod cap",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				maxPods := int32(50)
				spec.Kubelet = &v1alpha1.KubeletConfiguration{MaxPods: &maxPods}
			},
		},
		{
			name: "the bootstrap provider",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.Bootstrap = &v1alpha1.Bootstrap{Kubeadm: &v1alpha1.KubeadmBootstrap{}}
			},
		},
		{
			name: "bootstrap configuration",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.Bootstrap = &v1alpha1.Bootstrap{
					Kubeadm: &v1alpha1.KubeadmBootstrap{
						ConfigVersion: "kubeadm.k8s.io/v1beta3",
					},
				}
			},
		},
		{
			name: "the boot disk size",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.Storage.Boot.Size = *resourceQuantity("100Gi")
			},
		},
		{
			name: "a diskSet's size",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.Storage.DiskSets[0].Size = *resourceQuantity("200Gi")
			},
		},
		{
			name: "a diskSet's count",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.Storage.DiskSets[0].Count = 3
			},
		},
		{
			name: "a diskSet's type",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.Storage.DiskSets[0].Type = v1alpha1.DiskSetTypeLocal
			},
		},
		{
			name: "a diskSet's filesystem",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.Storage.DiskSets[0].Filesystem = v1alpha1.FilesystemXFS
			},
		},
		{
			name: "a diskSet's mount directory",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.Storage.DiskSets[0].MountDir = "/mnt/elsewhere"
			},
		},
		{
			name: "the kubelet reservations",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.Kubelet = &v1alpha1.KubeletConfiguration{
					SystemReserved: map[string]string{"memory": "1Gi"},
				}
			},
		},
		{
			name: "a diskSet being added",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.Storage.DiskSets = append(spec.Storage.DiskSets, v1alpha1.DiskSet{
					Name: "extra", Type: v1alpha1.DiskSetTypeLocal, Count: 1,
					Size: *resourceQuantity("10Gi"), MountDir: "/mnt/extra",
				})
			},
		},
		{
			name: "two diskSets swapping sizes",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.Storage.DiskSets = []v1alpha1.DiskSet{
					{
						Name: "aaa", Type: v1alpha1.DiskSetTypeDistributed, Count: 1,
						Size: *resourceQuantity("200Gi"), MountDir: "/mnt/aaa",
					},
					{
						Name: "bbb", Type: v1alpha1.DiskSetTypeDistributed, Count: 1,
						Size: *resourceQuantity("100Gi"), MountDir: "/mnt/bbb",
					},
				}
			},
		},
		{
			name: "jumboFrames",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				jumbo := true
				spec.JumboFrames = &jumbo
			},
		},
		{
			name: "cpuPlatform",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.CPUPlatform = "amd_turin"
			},
		},
	}

	base := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Bootstrap: &v1alpha1.Bootstrap{RKE2: &v1alpha1.RKE2Bootstrap{}},
			Storage: v1alpha1.Storage{
				Boot: v1alpha1.BootDisk{Size: *resourceQuantity("50Gi")},
				DiskSets: []v1alpha1.DiskSet{
					{
						Name: "aaa", Type: v1alpha1.DiskSetTypeDistributed, Count: 1,
						Size: *resourceQuantity("100Gi"), MountDir: "/mnt/aaa",
					},
					{
						Name: "bbb", Type: v1alpha1.DiskSetTypeDistributed, Count: 1,
						Size: *resourceQuantity("200Gi"), MountDir: "/mnt/bbb",
					},
				},
			},
		},
	}
	before := base.Hash()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeClass := base.DeepCopy()
			tt.mutate(&nodeClass.Spec)
			if after := nodeClass.Hash(); after == before {
				t.Errorf("Hash() = %q after changing %s, want it to differ", after, tt.name)
			}
		})
	}
}

func resourceQuantity(value string) *resource.Quantity {
	quantity := resource.MustParse(value)
	return &quantity
}

func TestHashIsStableAcrossQuantityFormats(t *testing.T) {
	binary := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Storage: v1alpha1.Storage{
				Boot: v1alpha1.BootDisk{Size: resource.MustParse("1Gi")},
			},
		},
	}
	decimal := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Storage: v1alpha1.Storage{
				Boot: v1alpha1.BootDisk{Size: *resource.NewQuantity(1<<30, resource.DecimalSI)},
			},
		},
	}
	if binary.Hash() != decimal.Hash() {
		t.Errorf(
			"Hash() = %q for 1Gi and %q for the same size written decimally, want them equal",
			binary.Hash(),
			decimal.Hash(),
		)
	}
}

func TestHashIgnoresDiskSetOrder(t *testing.T) {
	first := v1alpha1.DiskSet{
		Name: "aaa", Type: v1alpha1.DiskSetTypeDistributed, Count: 1,
		Size: *resourceQuantity("100Gi"), MountDir: "/mnt/aaa",
	}
	second := v1alpha1.DiskSet{
		Name: "bbb", Type: v1alpha1.DiskSetTypeLocal, Count: 4,
		Size: *resourceQuantity("500Gi"), MountDir: "/mnt/bbb",
	}
	nodeClassWith := func(sets ...v1alpha1.DiskSet) *v1alpha1.OxideNodeClass {
		return &v1alpha1.OxideNodeClass{
			Spec: v1alpha1.OxideNodeClassSpec{
				Storage: v1alpha1.Storage{
					Boot:     v1alpha1.BootDisk{Size: *resourceQuantity("20Gi")},
					DiskSets: sets,
				},
			},
		}
	}
	forward := nodeClassWith(first, second)
	reversed := nodeClassWith(second, first)
	if got, want := reversed.Hash(), forward.Hash(); got != want {
		t.Errorf("Hash() = %q with diskSets reversed and %q in order, want them equal", got, want)
	}
}

func TestHashSurvivesARoundTrip(t *testing.T) {
	nodeClass := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Storage: v1alpha1.Storage{
				Boot: v1alpha1.BootDisk{Size: *resourceQuantity("20Gi")},
				DiskSets: []v1alpha1.DiskSet{{
					Name: "local-pv", Type: v1alpha1.DiskSetTypeLocal, Count: 4,
					Size: *resourceQuantity("500Gi"), MountDir: "/mnt/local-pv",
				}},
			},
		},
	}
	encoded, err := json.Marshal(nodeClass)
	if err != nil {
		t.Fatalf("unable to marshal the nodeclass: %v", err)
	}
	roundTripped := &v1alpha1.OxideNodeClass{}
	if err := json.Unmarshal(encoded, roundTripped); err != nil {
		t.Fatalf("unable to unmarshal the nodeclass: %v", err)
	}
	if got, want := roundTripped.Hash(), nodeClass.Hash(); got != want {
		t.Errorf("Hash() = %q after a round trip and %q before, want them equal", got, want)
	}
}
