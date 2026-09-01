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
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

const (
	prefix     = "karpenter.oxide.computer/owner/v1"
	poolPrefix = "karpenter.oxide.computer/boot-pool/v1"
)

type Kind string

// RejectionReason is a low-cardinality explanation for an ownership mismatch.
type RejectionReason string

const (
	KindInstance Kind = "instance"
	KindBootDisk Kind = "boot-disk"
	KindDataDisk Kind = "data-disk"
)

const (
	ReasonUnmarked       RejectionReason = "unmarked"
	ReasonMalformed      RejectionReason = "malformed"
	ReasonForeignCluster RejectionReason = "foreign_cluster"
	ReasonWrongNodeClaim RejectionReason = "wrong_nodeclaim"
	ReasonWrongKind      RejectionReason = "wrong_kind"
)

// ErrOwnershipConflict reports only marker fields that are safe to expose in diagnostics.
type ErrOwnershipConflict struct {
	Reason   RejectionReason
	Expected Marker
	Observed Marker
}

func (e *ErrOwnershipConflict) Error() string {
	return fmt.Sprintf(
		"ownership conflict (%s): expected cluster=%q nodeclaim=%q name=%q kind=%q, "+
			"observed cluster=%q nodeclaim=%q name=%q kind=%q",
		e.Reason,
		e.Expected.ClusterUID,
		e.Expected.NodeClaimUID,
		e.Expected.NodeClaimName,
		e.Expected.Kind,
		e.Observed.ClusterUID,
		e.Observed.NodeClaimUID,
		e.Observed.NodeClaimName,
		e.Observed.Kind,
	)
}

// Conflict returns the typed ownership conflict wrapped by err, if any.
func Conflict(err error) (*ErrOwnershipConflict, bool) {
	var conflict *ErrOwnershipConflict
	return conflict, errors.As(err, &conflict)
}

// Marker identifies the cluster and immutable NodeClaim incarnation that owns one Oxide resource.
type Marker struct {
	ClusterUID   types.UID
	NodeClaimUID types.UID
	// NodeClaimName is carried alongside the UID because a UID is not addressable.
	NodeClaimName string
	Kind          Kind
}

type PoolMarker struct {
	ClusterUID   types.UID
	BootDiskUID  types.UID
	NodeClassUID types.UID
	ConfigHash   string
}

func (m PoolMarker) Description() string {
	return fmt.Sprintf(
		"%s cluster=%s bootdisk=%s nodeclass=%s config=%s",
		poolPrefix,
		m.ClusterUID,
		m.BootDiskUID,
		m.NodeClassUID,
		m.ConfigHash,
	)
}

func ParsePool(description string) (PoolMarker, bool) {
	fields := strings.Fields(description)
	if len(fields) != 5 || fields[0] != poolPrefix {
		return PoolMarker{}, false
	}
	cluster, ok := field(fields[1], "cluster")
	if !ok || cluster == "" {
		return PoolMarker{}, false
	}
	bootDisk, ok := field(fields[2], "bootdisk")
	if !ok || bootDisk == "" {
		return PoolMarker{}, false
	}
	nodeClass, ok := field(fields[3], "nodeclass")
	if !ok || nodeClass == "" {
		return PoolMarker{}, false
	}
	config, ok := field(fields[4], "config")
	if !ok || config == "" {
		return PoolMarker{}, false
	}
	return PoolMarker{
		ClusterUID:   types.UID(cluster),
		BootDiskUID:  types.UID(bootDisk),
		NodeClassUID: types.UID(nodeClass),
		ConfigHash:   config,
	}, true
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

// Verify requires a description marker to exactly match the expected owner.
func Verify(description string, expected Marker) error {
	observed, err := parseForVerification(description, expected)
	if err != nil {
		return err
	}
	if expected.ClusterUID == "" || expected.NodeClaimUID == "" ||
		expected.NodeClaimName == "" || !validKind(expected.Kind) {
		return &ErrOwnershipConflict{
			Reason:   ReasonMalformed,
			Expected: expected,
			Observed: observed,
		}
	}
	if observed.ClusterUID != expected.ClusterUID {
		return &ErrOwnershipConflict{
			Reason:   ReasonForeignCluster,
			Expected: expected,
			Observed: observed,
		}
	}
	if observed.NodeClaimUID != expected.NodeClaimUID ||
		observed.NodeClaimName != expected.NodeClaimName {
		return &ErrOwnershipConflict{
			Reason:   ReasonWrongNodeClaim,
			Expected: expected,
			Observed: observed,
		}
	}
	if observed.Kind != expected.Kind {
		return &ErrOwnershipConflict{
			Reason:   ReasonWrongKind,
			Expected: expected,
			Observed: observed,
		}
	}
	return nil
}

// VerifyClusterResource verifies the cluster and kind of a NodeClaim-owned resource.
func VerifyClusterResource(
	description string,
	clusterUID types.UID,
	kind Kind,
) (Marker, error) {
	expected := Marker{ClusterUID: clusterUID, Kind: kind}
	observed, err := parseForVerification(description, expected)
	if err != nil {
		return Marker{}, err
	}
	if clusterUID == "" || !validKind(kind) {
		return Marker{}, &ErrOwnershipConflict{
			Reason:   ReasonMalformed,
			Expected: expected,
			Observed: observed,
		}
	}
	if observed.ClusterUID != clusterUID {
		return Marker{}, &ErrOwnershipConflict{
			Reason: ReasonForeignCluster, Expected: expected, Observed: observed,
		}
	}
	if observed.Kind != kind {
		return Marker{}, &ErrOwnershipConflict{
			Reason: ReasonWrongKind, Expected: expected, Observed: observed,
		}
	}
	return observed, nil
}

func parseForVerification(description string, expected Marker) (Marker, error) {
	observed, ok := Parse(description)
	if ok {
		return observed, nil
	}
	reason := ReasonUnmarked
	if strings.HasPrefix(strings.TrimSpace(description), prefix) {
		reason = ReasonMalformed
	}
	return Marker{}, &ErrOwnershipConflict{Reason: reason, Expected: expected}
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
