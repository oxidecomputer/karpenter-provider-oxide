package cloudprovider

import (
	"context"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
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
	groupID   = "77777777-0000-4000-8000-000000000001"
	groupIDB  = "77777777-0000-4000-8000-000000000002"
)

// launched is a NodeClaim carrying the full launch record a current launch leaves behind, matching
// the nodeclass built by resolved(). Each case then perturbs one side of one comparison, so what a
// test is about is the difference from this pair rather than the whole of both.
func launched(hash string) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
			Annotations: map[string]string{
				v1alpha1.AnnotationOxideNodeClassHash:        hash,
				v1alpha1.AnnotationOxideNodeClassHashVersion: v1alpha1.OxideNodeClassHashVersion,
				v1alpha1.AnnotationSubnet:                    subnetID,
				v1alpha1.AnnotationSSHKeys:                   sshKeyID,
				v1alpha1.AnnotationIPPools:                   ipPoolID,
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

// resolved is the nodeclass the launch above came from: stamped with its hash, and publishing exactly
// the resolved state the record names.
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
			IPPools: []v1alpha1.OxideIPPool{{ID: ipPoolID}},
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
			want: DriftReasonNodeClass,
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
			want: DriftReasonImage,
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
			want: DriftReasonSubnet,
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
			want: DriftReasonSSHKey,
		},
		{
			name: "and an SSH key removed has to stop reaching it",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.SSHKeys = nil
			},
			want: DriftReasonSSHKey,
		},
		{
			name: "a node launched with no keys is not the same as one with no record of them",
			nodeClaim: func(nodeClaim *karpv1.NodeClaim) {
				nodeClaim.Annotations[v1alpha1.AnnotationSSHKeys] = ""
			},
			want: DriftReasonSSHKey,
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
			name: "an IP pool selection that no longer matches what was attached is drift",
			nodeClass: func(nodeClass *v1alpha1.OxideNodeClass) {
				nodeClass.Status.IPPools = nil
			},
			want: DriftReasonIPPool,
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
			want: DriftReasonAffinityGroup,
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
			want: DriftReasonNodeClass,
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

// A nodeclass that has been deleted is on its way to taking its nodes with it, so there is nothing to
// compare and nothing a replacement would achieve.
func TestIsDriftedWithoutANodeClass(t *testing.T) {
	c := &OxideCloudProvider{
		kubeClient: fake.NewClientBuilder().WithScheme(scheme.Scheme).Build(),
	}

	got, err := c.IsDrifted(context.Background(), launched("hash-1"))
	if err != nil {
		t.Fatalf("IsDrifted() error = %v", err)
	}
	if got != "" {
		t.Errorf("IsDrifted() = %q, want no drift", got)
	}
}

// The hash covers what a launch fixes into an instance, and nothing that is only selected. A selector
// rewritten to name the resource it already matched must not read as a spec edit, or every nodeclass
// tidied up in place would replace its fleet.
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
	nodeClass.Spec.IPPoolSelector = []v1alpha1.IPPoolSelector{{ID: ipPoolID}}
	if after := nodeClass.Hash(); after != before {
		t.Errorf("Hash() = %q after rewriting selectors, want the unchanged %q", after, before)
	}

	nodeClass.Spec.UserData = "#!/bin/sh\necho hello\n"
	if after := nodeClass.Hash(); after == before {
		t.Errorf("Hash() = %q after changing user data, want it to differ", after)
	}
}

// Every field the hash does cover, one at a time, since a field silently left out of it is a spec
// edit that replaces nothing and reports nothing.
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
			name: "kubelet args",
			mutate: func(spec *v1alpha1.OxideNodeClassSpec) {
				spec.KubeletArgs = []string{"max-pods=110"}
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
				spec.BootDiskSize = *resourceQuantity("100Gi")
			},
		},
	}

	base := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			Bootstrap:    &v1alpha1.Bootstrap{RKE2: &v1alpha1.RKE2Bootstrap{}},
			BootDiskSize: *resourceQuantity("50Gi"),
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

// resourceQuantity keeps the boot disk cases readable; a parse failure here is a broken test rather
// than a condition worth handling.
func resourceQuantity(value string) *resource.Quantity {
	quantity := resource.MustParse(value)
	return &quantity
}

// The boot disk size has to hash as the size it is rather than as the way it was written, since a
// quantity that has been through the API server comes back carrying a format the author never chose.
// Otherwise the same nodeclass hashes two ways and every node reads as drifted.
func TestHashIsStableAcrossQuantityFormats(t *testing.T) {
	binary := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{BootDiskSize: resource.MustParse("1Gi")},
	}
	decimal := &v1alpha1.OxideNodeClass{
		Spec: v1alpha1.OxideNodeClassSpec{
			BootDiskSize: *resource.NewQuantity(1<<30, resource.DecimalSI),
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
