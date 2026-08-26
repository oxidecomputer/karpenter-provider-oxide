/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package ownership manages Oxide resource ownership markers.
package ownership

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

const prefix = "karpenter.oxide.computer/owner/v1"

type Kind string

const (
	KindInstance Kind = "instance"
	KindBootDisk Kind = "boot-disk"
	KindDataDisk Kind = "data-disk"
)

// Marker identifies the cluster and immutable NodeClaim incarnation that owns one Oxide resource.
type Marker struct {
	ClusterUID   types.UID
	NodeClaimUID types.UID
	// NodeClaimName is carried alongside the UID because a UID is not addressable.
	NodeClaimName string
	Kind          Kind
}

// Description prepends a machine-readable marker while retaining the operator-facing description.
func (m Marker) Description(human string) string {
	marker := fmt.Sprintf(
		"%s cluster=%s nodeclaim=%s name=%s kind=%s",
		prefix,
		m.ClusterUID,
		m.NodeClaimUID,
		m.NodeClaimName,
		m.Kind,
	)
	if human == "" {
		return marker
	}
	return marker + "; " + human
}

// Parse reads a marker written by Description.
func Parse(description string) (Marker, bool) {
	markerText, _, _ := strings.Cut(description, ";")
	fields := strings.Fields(markerText)
	if len(fields) != 5 || fields[0] != prefix {
		return Marker{}, false
	}

	cluster, ok := field(fields[1], "cluster")
	if !ok || cluster == "" {
		return Marker{}, false
	}
	nodeClaim, ok := field(fields[2], "nodeclaim")
	if !ok || nodeClaim == "" {
		return Marker{}, false
	}
	name, ok := field(fields[3], "name")
	if !ok || name == "" {
		return Marker{}, false
	}
	kind, ok := field(fields[4], "kind")
	if !ok || !validKind(Kind(kind)) {
		return Marker{}, false
	}
	return Marker{
		ClusterUID:    types.UID(cluster),
		NodeClaimUID:  types.UID(nodeClaim),
		NodeClaimName: name,
		Kind:          Kind(kind),
	}, true
}

func field(value, name string) (string, bool) {
	prefix := name + "="
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	return strings.TrimPrefix(value, prefix), true
}

func validKind(kind Kind) bool {
	switch kind {
	case KindInstance, KindBootDisk, KindDataDisk:
		return true
	default:
		return false
	}
}
