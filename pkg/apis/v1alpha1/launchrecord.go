package v1alpha1

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// The launch record is what a NodeClaim remembers about the resolved state it was launched from: the
// subnet its interface sits in, the SSH keys copied into it, the IP pools its external addresses came
// from. None of it can be read back off the instance in a form that answers the drift question -- a
// pool is not recoverable from an address, and a key is not recoverable from an authorized_keys file
// nobody promises stayed untouched -- so it is recorded at launch, in the same style as
// AnnotationAffinityGroups.
//
// An absent record is not an empty one. It means the launch happened before this provider recorded
// anything, so drift has nothing to compare and reports none, which is what keeps a fleet from being
// replaced by an upgrade of Karpenter itself.

// EncodeIDs renders a set of Oxide resource IDs as an annotation value: sorted, deduplicated, and
// comma-separated. Sorted so that the same set always produces the same value rather than looking
// like a change because a list came back in a different order.
func EncodeIDs(ids []string) string {
	entries := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			entries = append(entries, id)
		}
	}
	slices.Sort(entries)
	return strings.Join(slices.Compact(entries), ",")
}

// ParseIDs reads an EncodeIDs value back into the set it records. Empty entries are dropped, so an
// empty or malformed value is the empty set rather than a set containing nothing useful.
func ParseIDs(value string) sets.Set[string] {
	ids := sets.New[string]()
	for entry := range strings.SplitSeq(value, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			ids.Insert(entry)
		}
	}
	return ids
}

// SubnetIDs is the set of subnets a launch could have placed an interface in.
func SubnetIDs(subnets []OxideSubnet) sets.Set[string] {
	ids := sets.New[string]()
	for _, subnet := range subnets {
		ids.Insert(subnet.ID)
	}
	return ids
}

// SSHKeyIDs is the resolved SSH keys, in the form the launch record stores them.
func SSHKeyIDs(sshKeys []OxideSSHKey) []string {
	ids := make([]string, 0, len(sshKeys))
	for _, sshKey := range sshKeys {
		ids = append(ids, sshKey.ID)
	}
	return ids
}

// IPPoolIDs is the resolved IP pools, in the form the launch record stores them.
func IPPoolIDs(ipPools []OxideIPPool) []string {
	ids := make([]string, 0, len(ipPools))
	for _, ipPool := range ipPools {
		ids = append(ids, ipPool.ID)
	}
	return ids
}
