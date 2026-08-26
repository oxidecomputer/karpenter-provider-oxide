package v1alpha1

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// The launch record is what a NodeClaim remembers about the resolved state it was launched from: the
// subnet its interface sits in, the SSH keys copied into it, the external addresses attached to it.
// None of it can be read back off the instance in a form that answers the drift question -- a
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

// EphemeralIPRecord is one external address an instance was launched with, as the launch record
// remembers it: which family it belongs to, the address itself, and the pool it actually came from.
//
// The pool is ground truth read back off the instance rather than what the nodeclass asked for,
// which is the only way an entry that took the silo's default records a pool at all.
type EphemeralIPRecord struct {
	IPVersion string
	Address   string
	PoolID    string
}

// EncodeEphemeralIPs renders the addresses an instance launched with as an annotation value:
// "<version>=<address>@<pool-uuid>" entries, sorted by version and comma-separated.
//
// Keyed by version rather than written as an unordered set of pool IDs because Oxide gives an
// instance at most one ephemeral address per version, so the version is the slot each entry
// occupies. It is also what makes swapping which pool serves v4 and which serves v6 visible, which
// a set of IDs cannot see.
func EncodeEphemeralIPs(records []EphemeralIPRecord) string {
	entries := make([]string, 0, len(records))
	for _, record := range records {
		version := strings.TrimSpace(record.IPVersion)
		if version == "" {
			continue
		}
		entries = append(
			entries,
			version+"="+strings.TrimSpace(record.Address)+"@"+strings.TrimSpace(record.PoolID),
		)
	}
	slices.Sort(entries)
	return strings.Join(slices.Compact(entries), ",")
}

// ParseEphemeralIPs reads an EncodeEphemeralIPs value back into the records it holds, keyed by IP
// version. An entry with no "=" is dropped, since it says nothing about which slot it filled; the
// "@<pool>" half is optional so that an address whose pool was not recorded still names its version.
func ParseEphemeralIPs(value string) map[string]EphemeralIPRecord {
	records := map[string]EphemeralIPRecord{}
	for entry := range strings.SplitSeq(value, ",") {
		version, rest, found := strings.Cut(strings.TrimSpace(entry), "=")
		if !found {
			continue
		}
		if version = strings.TrimSpace(version); version == "" {
			continue
		}
		address, pool, _ := strings.Cut(rest, "@")
		records[version] = EphemeralIPRecord{
			IPVersion: version,
			Address:   strings.TrimSpace(address),
			PoolID:    strings.TrimSpace(pool),
		}
	}
	return records
}

// EncodeDiskSetSources renders the source each diskSet's disks were created from as an annotation
// value: "<set>=<uuid>" entries, sorted by set name and comma-separated.
//
// A set with no source records an empty value rather than being left out. That is the same
// distinction EncodeIDs draws for SSH keys: an entry with an empty value is a set whose disks were
// created blank, and no entry at all is a set nothing was recorded for. The first can drift when a
// selector is added; the second cannot be compared and never does.
func EncodeDiskSetSources(sources map[string]string) string {
	entries := make([]string, 0, len(sources))
	for set, id := range sources {
		if set = strings.TrimSpace(set); set != "" {
			entries = append(entries, set+"="+strings.TrimSpace(id))
		}
	}
	slices.Sort(entries)
	return strings.Join(entries, ",")
}

// ParseDiskSetSources reads an EncodeDiskSetSources value back into the map it records. An entry with
// no "=" is dropped, since it says nothing about which set it belonged to.
func ParseDiskSetSources(value string) map[string]string {
	sources := map[string]string{}
	for entry := range strings.SplitSeq(value, ",") {
		set, id, found := strings.Cut(strings.TrimSpace(entry), "=")
		if !found {
			continue
		}
		if set = strings.TrimSpace(set); set != "" {
			sources[set] = strings.TrimSpace(id)
		}
	}
	return sources
}

// DiskSetSources is the source each resolved diskSet's disks would be created from now, in the form
// the launch record stores them. An image wins over a snapshot, which CEL makes mutually exclusive
// anyway.
func DiskSetSources(status *StorageStatus) map[string]string {
	sources := map[string]string{}
	if status == nil {
		return sources
	}
	for _, set := range status.DiskSets {
		switch {
		case len(set.Images) > 0:
			sources[set.Name] = set.Images[0].ID
		case len(set.Snapshots) > 0:
			sources[set.Name] = set.Snapshots[0].ID
		default:
			sources[set.Name] = ""
		}
	}
	return sources
}
