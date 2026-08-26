package v1alpha1

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// EncodeIDs renders a set of Oxide resource IDs as an annotation value: sorted, deduplicated, and comma-separated.
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

// ParseIDs reads an EncodeIDs value back into the set it records.
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

// EphemeralIPRecord is one external address an instance was launched with.
type EphemeralIPRecord struct {
	IPVersion string
	Address   string
	PoolID    string
}

// EncodeEphemeralIPs renders the addresses an instance launched with as an annotation value.
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

// ParseEphemeralIPs reads an EncodeEphemeralIPs value back into the records it holds, keyed by IP version.
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

// EncodeDiskSetSources renders the source each diskSet's disks were created from as an annotation value.
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

// ParseDiskSetSources reads an EncodeDiskSetSources value back into the map it records.
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

// DiskSetSources is the source each resolved diskSet's disks would be created from now.
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
