package v1alpha1

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// Type codes as they appear in a managed group's name and in the AnnotationAffinityGroups value.
const (
	AffinityGroupCodeAffinity     = "af"
	AffinityGroupCodeAntiAffinity = "aa"
)

// AffinityGroupRef is one group membership as it was recorded on a NodeClaim at launch.
type AffinityGroupRef struct {
	// Type is Affinity or AntiAffinity.
	Type string
	// ID identifies the group.
	ID string
}

// AffinityGroupTypeCode is the two-character code for a group kind.
func AffinityGroupTypeCode(groupType string) string {
	if groupType == AffinityGroupTypeAffinity {
		return AffinityGroupCodeAffinity
	}
	return AffinityGroupCodeAntiAffinity
}

// AffinityGroupTypeForCode is the inverse of AffinityGroupTypeCode.
func AffinityGroupTypeForCode(code string) string {
	switch code {
	case AffinityGroupCodeAffinity:
		return AffinityGroupTypeAffinity
	case AffinityGroupCodeAntiAffinity:
		return AffinityGroupTypeAntiAffinity
	default:
		return ""
	}
}

// EncodeAffinityGroups renders group memberships as the AnnotationAffinityGroups value.
func EncodeAffinityGroups(refs []AffinityGroupRef) string {
	entries := make([]string, 0, len(refs))
	for _, ref := range refs {
		entries = append(entries, AffinityGroupTypeCode(ref.Type)+":"+ref.ID)
	}
	slices.Sort(entries)
	return strings.Join(slices.Compact(entries), ",")
}

// ParseAffinityGroups reads an AnnotationAffinityGroups value back into the memberships it records.
func ParseAffinityGroups(value string) []AffinityGroupRef {
	var refs []AffinityGroupRef
	for entry := range strings.SplitSeq(value, ",") {
		code, id, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || id == "" {
			continue
		}
		refs = append(refs, AffinityGroupRef{Type: AffinityGroupTypeForCode(code), ID: id})
	}
	return refs
}

// AffinityGroupIDs is the set of group IDs in a resolved desired set.
func AffinityGroupIDs(groups []OxideAffinityGroup) sets.Set[string] {
	ids := sets.New[string]()
	for _, group := range groups {
		ids.Insert(group.ID)
	}
	return ids
}

// AffinityGroupRefsFor renders a resolved desired set as the memberships a launch records for it.
func AffinityGroupRefsFor(groups []OxideAffinityGroup) []AffinityGroupRef {
	refs := make([]AffinityGroupRef, 0, len(groups))
	for _, group := range groups {
		refs = append(refs, AffinityGroupRef{Type: group.Type, ID: group.ID})
	}
	return refs
}
