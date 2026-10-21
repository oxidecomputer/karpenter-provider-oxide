// Package snapshot resolves snapshot selectors.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/resolver"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Result is one resolution of a diskSet's snapshot selectors.
type Result = resolver.Resolution[v1alpha1.OxideSnapshot, v1alpha1.SnapshotSelector]

// Provider is everything that talks to the Oxide API about snapshots.
type Provider struct {
	ox       *oxide.Client
	projects *project.Resolver
	cache    *resolver.Cache[reading]
}

// reading is one resolution as cached: the snapshots matched, and the selectors that found their
// snapshot in a project other than the one they meant.
type reading struct {
	snapshots []v1alpha1.OxideSnapshot
	misplaced []string
}

// NewProvider returns a provider that re-resolves a selector's snapshots once its reading is older than ttl.
func NewProvider(ox *oxide.Client, projects *project.Resolver, ttl time.Duration) *Provider {
	return &Provider{
		ox:       ox,
		projects: projects,
		cache:    resolver.NewCache[reading]("snapshot", ttl),
	}
}

// Resolve returns the snapshots the selectors match, most recently created first.
func (p *Provider) Resolve(
	ctx context.Context,
	selectors []v1alpha1.SnapshotSelector,
) (Result, error) {
	if len(selectors) == 0 {
		return Result{}, nil
	}
	entry, err := p.cache.Get(
		ctx,
		resolver.Fingerprint(selectors),
		func(ctx context.Context) (reading, error) {
			return p.resolve(ctx, selectors)
		},
	)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Resolved: slices.Clone(entry.Value.snapshots),
		Age:      entry.Age(time.Now()),
		Stale:    entry.Stale,
	}

	switch {
	case len(entry.Value.misplaced) > 0:
		result.Reason = v1alpha1.ConditionReasonProjectMismatch
		result.Message = strings.Join(entry.Value.misplaced, "; ")
	case len(result.Resolved) == 0:
		result.Unmatched = selectors
		result.Reason = v1alpha1.ConditionReasonSnapshotsNotFound
		result.Message = DescribeSelectors(selectors)
	}
	return result, nil
}

func (p *Provider) resolve(
	ctx context.Context,
	selectors []v1alpha1.SnapshotSelector,
) (reading, error) {
	var out reading
	var resolved []oxide.Snapshot
	var listSelectors []v1alpha1.SnapshotSelector

	for _, selector := range selectors {
		nameOrID := ""
		switch {
		case selector.ID != "":
			nameOrID = string(selector.ID)
		case selector.Name != "" && !selector.Name.IsPattern():
			nameOrID = string(selector.Name)
		default:
			listSelectors = append(listSelectors, selector)
			continue
		}
		in := project.Scope(p.projects.For(selector.Project), nameOrID)
		snapshot, err := p.ox.SnapshotView(ctx, oxide.SnapshotViewParams{
			Snapshot: oxide.NameOrId(nameOrID),
			Project:  in,
		})
		if err != nil {
			if errors.Is(err, oxide.ErrObjectNotFound) {
				continue
			}
			return reading{}, fmt.Errorf("snapshot %q from project %q: %w", nameOrID, in, err)
		}
		if selector.Name != "" && !selector.Name.Matches(string(snapshot.Name)) {
			continue
		}
		if selector.ID != "" {
			intended := p.projects.For(selector.Project)
			ok, err := p.projects.Matches(ctx, intended, snapshot.ProjectId)
			if err != nil {
				return reading{}, err
			}
			if !ok {
				out.misplaced = append(out.misplaced, fmt.Sprintf(
					"snapshot %s is in project %s, not project %s",
					snapshot.Id, snapshot.ProjectId, intended,
				))
				continue
			}
		}
		resolved = append(resolved, *snapshot)
	}

	for in, grouped := range lo.GroupBy(
		listSelectors,
		func(selector v1alpha1.SnapshotSelector) string { return p.projects.For(selector.Project) },
	) {
		snapshots, err := p.ox.SnapshotListAllPages(ctx, oxide.SnapshotListParams{
			Project: oxide.NameOrId(in),
		})
		if err != nil {
			return reading{}, fmt.Errorf("unable to list snapshots in project %q: %w", in, err)
		}
		for _, selector := range grouped {
			for _, snapshot := range snapshots {
				if selector.Name != "" && !selector.Name.Matches(string(snapshot.Name)) {
					continue
				}
				resolved = append(resolved, snapshot)
			}
		}
	}

	resolved = lo.Filter(resolved, func(snapshot oxide.Snapshot, _ int) bool {
		return snapshot.State == oxide.SnapshotStateReady
	})
	resolved = lo.UniqBy(resolved, func(snapshot oxide.Snapshot) string { return snapshot.Id })

	slices.SortStableFunc(resolved, func(a, b oxide.Snapshot) int {
		if a.TimeCreated == nil || b.TimeCreated == nil {
			return strings.Compare(b.Id, a.Id)
		}
		if b.TimeCreated.Compare(*a.TimeCreated) == 0 {
			return strings.Compare(b.Id, a.Id)
		}
		return b.TimeCreated.Compare(*a.TimeCreated)
	})

	out.snapshots = lo.Map(resolved, func(snapshot oxide.Snapshot, _ int) v1alpha1.OxideSnapshot {
		s := v1alpha1.OxideSnapshot{
			ID:        snapshot.Id,
			Name:      string(snapshot.Name),
			Project:   snapshot.ProjectId,
			DiskID:    snapshot.DiskId,
			SizeBytes: int64(snapshot.Size),
		}
		if snapshot.TimeCreated != nil {
			s.TimeCreated = metav1.NewTime(*snapshot.TimeCreated)
		}
		return s
	})
	return out, nil
}

// DescribeSelectors renders snapshot selectors back as the terms they were written with.
func DescribeSelectors(selectors []v1alpha1.SnapshotSelector) string {
	if len(selectors) == 0 {
		return "no snapshot selectors are configured"
	}
	described := lo.Map(selectors, func(selector v1alpha1.SnapshotSelector, _ int) string {
		terms := []string{}
		for _, term := range []struct{ key, value string }{
			{"id", string(selector.ID)},
			{"name", string(selector.Name)},
			{"project", selector.Project},
		} {
			if term.value != "" {
				terms = append(terms, fmt.Sprintf("%s=%s", term.key, term.value))
			}
		}
		if len(terms) == 0 {
			return "{}"
		}
		return "{" + strings.Join(terms, ",") + "}"
	})
	return "no snapshots matched " + strings.Join(described, " or ")
}
