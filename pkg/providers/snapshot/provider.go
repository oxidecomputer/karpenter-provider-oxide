// Package snapshot resolves a diskSet's snapshot selectors into the snapshots its disks would be
// created from.
//
// It mirrors pkg/providers/image closely, and differs only where the Oxide API forces it to: a
// snapshot carries no operating system and no version, and it is always inside a project.
//
// Resolution is cached for the same reason images are: the selectors are re-evaluated on every
// reconcile, and several diskSets selecting the same snapshot should not each list the project's
// snapshots. The cache window is shorter than the resync, so every resync lists again; see
// providers.CoalesceWindow.
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
	// project is where a selector that names none looks. Unlike an image selector, a snapshot
	// selector cannot mean "the silo": there is no silo-level snapshot in the Oxide API, so an empty
	// project falls back to the one Karpenter runs in rather than to nothing.
	project string
	cache   *resolver.Cache[[]v1alpha1.OxideSnapshot]
}

// NewProvider returns a provider that re-resolves a selector's snapshots once its reading is older
// than ttl.
func NewProvider(ox *oxide.Client, project string, ttl time.Duration) *Provider {
	return &Provider{
		ox:      ox,
		project: project,
		cache:   resolver.NewCache[[]v1alpha1.OxideSnapshot]("snapshot", ttl),
	}
}

// Resolve returns the snapshots the selectors match, most recently created first.
//
// It takes the selectors rather than a nodeclass because a snapshot selector belongs to one diskSet
// and there can be several. That keeps the cache key over exactly the question being asked -- a
// fingerprint must never be taken over a whole nodeclass -- and lets two diskSets carrying the same
// selector share one entry and one listing.
//
// The whole matched set is returned rather than only the snapshot that will be used. Which snapshots
// lost is what makes a wildcard selector auditable from the nodeclass, and the caller decides how
// many of them are worth the object size.
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
		// Cloned for the reason the image provider clones: the cache keeps serving this reading and
		// the caller publishes it into an object it then patches.
		Resolved: slices.Clone(entry.Value),
		Age:      entry.Age(time.Now()),
		Stale:    entry.Stale,
	}
	// Nothing here records which selector matched which snapshot, so a resolution that found nothing
	// reports every selector as unmatched: with no snapshots at all, every one of them missed.
	if len(result.Resolved) == 0 {
		result.Unmatched = selectors
	}
	return result, nil
}

// resolve evaluates the selectors against the Oxide API. An ID, or a name with no "*", is looked up
// directly because that is the only form the API can resolve; everything else is matched against a
// listing.
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
			// A project cannot be used to query alongside an ID.
			project = ""
		} else if selector.Name != "" && !strings.Contains(selector.Name, "*") {
			nameOrID = selector.Name
		} else {
			// A wildcard name, or a selector with no name at all, is not something the API can look
			// up, so it is resolved by listing and matching.
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

	// No selector here has an ID or an exact name, else it would have been looked up directly above.
	// Grouped by project so each project is listed once however many selectors point at it.
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

	// Only a ready snapshot can back a disk. A creating, faulted or destroyed one would be refused by
	// the disk create with nothing on the nodeclass to explain it, so it is filtered here instead and
	// reads as "matched nothing", which the condition then reports.
	resolved = lo.Filter(resolved, func(snapshot oxide.Snapshot, _ int) bool {
		return snapshot.State == oxide.SnapshotStateReady
	})
	resolved = lo.UniqBy(resolved, func(snapshot oxide.Snapshot) string { return snapshot.Id })

	// Most recently created first, so a wildcard means "newest" here exactly as it does for images.
	// The ID breaks ties so the order is total rather than dependent on listing order.
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

// projectFor is the project a selector looks in: its own, or the one Karpenter runs in.
func (p *Provider) projectFor(selector v1alpha1.SnapshotSelector) string {
	if selector.Project != "" {
		return selector.Project
	}
	return p.project
}

// DescribeSelectors renders snapshot selectors back as the terms they were written with, so the
// StorageResolved condition can say what was looked for rather than only that nothing was found.
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
