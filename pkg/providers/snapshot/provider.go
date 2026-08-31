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
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/resolver"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Result is one resolution of a diskSet's snapshot selectors.
type Result = resolver.Resolution[v1alpha1.OxideSnapshot, v1alpha1.SnapshotSelector]

// Provider is everything that talks to the Oxide API about snapshots.
type Provider struct {
	ox *oxide.Client

	project string
	cache   *resolver.Cache[[]v1alpha1.OxideSnapshot]
}

// NewProvider returns a provider that re-resolves a selector's snapshots once its reading is older than ttl.
func NewProvider(ox *oxide.Client, project string, ttl time.Duration) *Provider {
	return &Provider{
		ox:      ox,
		project: project,
		cache:   resolver.NewCache[[]v1alpha1.OxideSnapshot]("snapshot", ttl),
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
		func(ctx context.Context) ([]v1alpha1.OxideSnapshot, error) {
			return p.resolve(ctx, selectors)
		},
	)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Resolved: slices.Clone(entry.Value),
		Age:      entry.Age(time.Now()),
		Stale:    entry.Stale,
	}

	if len(result.Resolved) == 0 {
		result.Unmatched = selectors
	}
	return result, nil
}

func (p *Provider) resolve(
	ctx context.Context,
	selectors []v1alpha1.SnapshotSelector,
) ([]v1alpha1.OxideSnapshot, error) {
	var resolved []oxide.Snapshot
	var listSelectors []v1alpha1.SnapshotSelector

	for _, selector := range selectors {
		nameOrID := ""
		project := p.projectFor(selector)
		if selector.ID != "" {
			nameOrID = selector.ID
			project = ""
		} else if selector.Name != "" && !strings.Contains(selector.Name, "*") {
			nameOrID = selector.Name
		} else {
			listSelectors = append(listSelectors, selector)
			continue
		}
		snapshot, err := p.ox.SnapshotView(ctx, oxide.SnapshotViewParams{
			Snapshot: oxide.NameOrId(nameOrID),
			Project:  oxide.NameOrId(project),
		})
		if err != nil {
			if errors.Is(err, oxide.ErrObjectNotFound) {
				continue
			}
			return nil, fmt.Errorf("snapshot %q from project %q: %w", nameOrID, project, err)
		}
		resolved = append(resolved, *snapshot)
	}

	for project, grouped := range lo.GroupBy(
		listSelectors,
		func(selector v1alpha1.SnapshotSelector) string { return p.projectFor(selector) },
	) {
		snapshots, err := p.ox.SnapshotListAllPages(ctx, oxide.SnapshotListParams{
			Project: oxide.NameOrId(project),
		})
		if err != nil {
			return nil, fmt.Errorf("unable to list snapshots in project %q: %w", project, err)
		}
		for _, selector := range grouped {
			for _, snapshot := range snapshots {
				if selector.Name != "" &&
					!resolver.MatchName(selector.Name, string(snapshot.Name)) {
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

	return lo.Map(resolved, func(snapshot oxide.Snapshot, _ int) v1alpha1.OxideSnapshot {
		out := v1alpha1.OxideSnapshot{
			ID:        snapshot.Id,
			Name:      string(snapshot.Name),
			Project:   snapshot.ProjectId,
			DiskID:    snapshot.DiskId,
			SizeBytes: int64(snapshot.Size),
		}
		if snapshot.TimeCreated != nil {
			out.TimeCreated = metav1.NewTime(*snapshot.TimeCreated)
		}
		return out
	}), nil
}

func (p *Provider) projectFor(selector v1alpha1.SnapshotSelector) string {
	if selector.Project != "" {
		return selector.Project
	}
	return p.project
}

// DescribeSelectors renders snapshot selectors back as the terms they were written with.
func DescribeSelectors(selectors []v1alpha1.SnapshotSelector) string {
	if len(selectors) == 0 {
		return "no snapshot selectors are configured"
	}
	described := lo.Map(selectors, func(selector v1alpha1.SnapshotSelector, _ int) string {
		terms := []string{}
		for _, term := range []struct{ key, value string }{
			{"id", selector.ID},
			{"name", selector.Name},
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
