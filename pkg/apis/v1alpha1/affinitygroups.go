package v1alpha1

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// Type codes as they appear in a managed group's name and in the AnnotationAffinityGroups value. Two
// characters, because in a group name every character spent here is one taken from the nodeclass
// name the user chose, and there is no reason for the annotation to spell it differently.
const (
	AffinityGroupCodeAffinity     = "af"
	AffinityGroupCodeAntiAffinity = "aa"
)

// AffinityGroupRef is one group membership as it was recorded on a NodeClaim at launch.
type AffinityGroupRef struct {
	// Type is Affinity or AntiAffinity. It is carried because a group that has since left the
	// desired set still has to be addressed on the right endpoint in order to leave it, and by then
	// the nodeclass no longer says which kind it was.
	Type string
	// ID identifies the group. Names are deliberately not recorded: a group deleted and recreated
	// under the same name is a different group that this instance is not a member of, and recording
	// IDs is what makes that visible rather than silently true.
	ID string
}

// AffinityGroupTypeCode is the two-character code for a group kind.
func AffinityGroupTypeCode(groupType string) string {
	if groupType == AffinityGroupTypeAffinity {
		return AffinityGroupCodeAffinity
	}
	return AffinityGroupCodeAntiAffinity
}

// AffinityGroupTypeForCode is the inverse of AffinityGroupTypeCode. An unrecognized code returns an
// empty string, so a value written by a newer version is left alone rather than acted on as though
// it were anti-affinity.
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

// EncodeAffinityGroups renders group memberships as the AnnotationAffinityGroups value: "<code>:<id>"
// entries, sorted, comma-separated. Sorted so that the same set of memberships always produces the
// same value and does not look like a change.
func EncodeAffinityGroups(refs []AffinityGroupRef) string {
	entries := make([]string, 0, len(refs))
	for _, ref := range refs {
		entries = append(entries, AffinityGroupTypeCode(ref.Type)+":"+ref.ID)
	}
	slices.Sort(entries)
	return strings.Join(slices.Compact(entries), ",")
}

// ParseAffinityGroups reads an AnnotationAffinityGroups value back into the memberships it records.
// Entries that are not in the expected form are dropped: the annotation is only ever written by this
// provider, so a malformed one says nothing worth acting on.
func ParseAffinityGroups(value string) []AffinityGroupRef {
	var refs []AffinityGroupRef
	for _, entry := range strings.Split(value, ",") {
		code, id, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || id == "" {
			continue
		}
		refs = append(refs, AffinityGroupRef{Type: AffinityGroupTypeForCode(code), ID: id})
	}
	return refs
}

// AffinityGroupIDs is the set of group IDs in a resolved desired set, which is what drift and
// membership removal are decided against.
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
