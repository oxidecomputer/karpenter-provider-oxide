package v1alpha1

import (
	"reflect"
	"testing"
)

func TestEncodeAffinityGroups(t *testing.T) {
	tests := []struct {
		name string
		refs []AffinityGroupRef
		want string
	}{
		{
			name: "nothing joined encodes to nothing",
		},
		{
			name: "the type is encoded so the group can be left on the right endpoint",
			refs: []AffinityGroupRef{{Type: AffinityGroupTypeAntiAffinity, ID: "b"}},
			want: "aa:b",
		},
		{
			name: "entries are sorted, so the same memberships never look like a change",
			refs: []AffinityGroupRef{
				{Type: AffinityGroupTypeAntiAffinity, ID: "c"},
				{Type: AffinityGroupTypeAffinity, ID: "a"},
				{Type: AffinityGroupTypeAntiAffinity, ID: "b"},
			},
			want: "aa:b,aa:c,af:a",
		},
		{
			name: "a group joined twice is one membership",
			refs: []AffinityGroupRef{
				{Type: AffinityGroupTypeAntiAffinity, ID: "b"},
				{Type: AffinityGroupTypeAntiAffinity, ID: "b"},
			},
			want: "aa:b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EncodeAffinityGroups(tt.refs); got != tt.want {
				t.Errorf("EncodeAffinityGroups() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseAffinityGroups(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []AffinityGroupRef
	}{
		{
			name: "an absent annotation is the empty set, which is what makes it drift-neutral",
		},
		{
			name:  "both kinds round-trip",
			value: "aa:b,af:a",
			want: []AffinityGroupRef{
				{Type: AffinityGroupTypeAntiAffinity, ID: "b"},
				{Type: AffinityGroupTypeAffinity, ID: "a"},
			},
		},
		{
			name:  "an unrecognized code keeps its ID and loses its kind",
			value: "zz:b",
			want:  []AffinityGroupRef{{Type: "", ID: "b"}},
		},
		{
			name:  "entries without an ID are dropped",
			value: "aa:,aa,aa:b",
			want:  []AffinityGroupRef{{Type: AffinityGroupTypeAntiAffinity, ID: "b"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseAffinityGroups(tt.value); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseAffinityGroups() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestAffinityGroupsRoundTrip(t *testing.T) {
	groups := []OxideAffinityGroup{
		{ID: "b", Type: AffinityGroupTypeAntiAffinity},
		{ID: "a", Type: AffinityGroupTypeAffinity},
	}
	refs := ParseAffinityGroups(EncodeAffinityGroups(AffinityGroupRefsFor(groups)))
	want := []AffinityGroupRef{
		{Type: AffinityGroupTypeAntiAffinity, ID: "b"},
		{Type: AffinityGroupTypeAffinity, ID: "a"},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("round trip = %#v, want %#v", refs, want)
	}
}
