package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
)

// The value has to be a function of the set alone. A resolution that came back in a different order,
// or that listed the same resource twice, is not a change to what the node was launched with, and an
// annotation that said otherwise would replace the node over it.
func TestEncodeIDs(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want string
	}{
		{name: "nothing selected encodes as the empty value", ids: nil, want: ""},
		{name: "one ID is itself", ids: []string{"b"}, want: "b"},
		{name: "order does not survive", ids: []string{"c", "a", "b"}, want: "a,b,c"},
		{name: "nor do duplicates", ids: []string{"a", "b", "a"}, want: "a,b"},
		{name: "empty entries are dropped", ids: []string{"a", "", " "}, want: "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EncodeIDs(tt.ids); got != tt.want {
				t.Errorf("EncodeIDs(%q) = %q, want %q", tt.ids, got, tt.want)
			}
		})
	}
}

func TestParseIDs(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  sets.Set[string]
	}{
		{
			name:  "a node launched with nothing selected reads back as the empty set",
			value: "",
			want:  sets.New[string](),
		},
		{
			name:  "a record reads back as the set it encoded",
			value: "a,b",
			want:  sets.New("a", "b"),
		},
		{
			name:  "and is not sensitive to how it was spaced",
			value: " a , b ",
			want:  sets.New("a", "b"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseIDs(tt.value); !got.Equal(tt.want) {
				t.Errorf(
					"ParseIDs(%q) = %v, want %v",
					tt.value,
					got.UnsortedList(),
					tt.want.UnsortedList(),
				)
			}
		})
	}
}
