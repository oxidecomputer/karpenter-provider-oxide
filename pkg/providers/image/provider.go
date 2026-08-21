// Package image resolves a nodeclass's image selectors into the images it would launch from, and
// decides whether a node running an older one has drifted.
//
// Resolution is cached because the selectors are re-evaluated on every reconcile and two nodeclasses
// selecting the same images should not each list the project's images. The cache does not decide how
// soon a newly uploaded image is picked up -- its window is shorter than the resync, so every resync
// lists again; see providers.CoalesceWindow.
package image

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/resolver"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// Result is one resolution of a nodeclass's image selectors.
type Result = resolver.Resolution[v1alpha1.OxideImage, v1alpha1.ImageSelector]

// Provider is everything that talks to the Oxide API about images.
type Provider struct {
	ox    *oxide.Client
	cache *resolver.Cache[[]v1alpha1.OxideImage]
}

// NewProvider returns a provider that re-resolves a nodeclass's images once its reading is older
// than ttl.
//
// No project is taken, unlike the other providers: an image selector carries its own project, and an
// image with no project is a silo image, so there is nothing left for a provider-wide project to
// scope.
func NewProvider(ox *oxide.Client, ttl time.Duration) *Provider {
	return &Provider{ox: ox, cache: resolver.NewCache[[]v1alpha1.OxideImage]("image", ttl)}
}

// Resolve returns the images the nodeclass's selectors match, newest first.
//
// The whole matched set is returned rather than only the image that will launch. Which images lost
// is what makes a wildcard selector auditable from the nodeclass, and the caller decides how many of
// them are worth the object size.
func (p *Provider) Resolve(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (Result, error) {
	selectors := nodeClass.Spec.ImageSelector
	entry, err := p.cache.Get(
		ctx,
		resolver.Fingerprint(selectors),
		func(ctx context.Context) ([]v1alpha1.OxideImage, error) {
			return p.resolve(ctx, selectors)
		},
	)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		// Cloned because the cache keeps serving this reading and the caller publishes it into an
		// object it then patches. Single-digit slices, so the copy costs nothing worth reasoning
		// about, whereas handing out the cache's own backing array does.
		Resolved: slices.Clone(entry.Value),
		Age:      entry.Age(time.Now()),
		Stale:    entry.Stale,
	}
	// Nothing here records which selector matched which image, so a resolution that found nothing
	// reports every selector as unmatched: with no images at all, every one of them missed.
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
	imageSelectors []v1alpha1.ImageSelector,
) ([]v1alpha1.OxideImage, error) {
	var resolvedImages []oxide.Image
	var listImageSelector []v1alpha1.ImageSelector
	// Check for direct query images via name or ID
	for _, imageSelector := range imageSelectors {
		nameOrID := ""
		project := imageSelector.Project
		if imageSelector.ID != "" {
			nameOrID = imageSelector.ID
			// project cannot be used to query if ID is used
			project = ""
		} else if imageSelector.Name != "" && !strings.Contains(imageSelector.Name, "*") {
			nameOrID = imageSelector.Name
		} else {
			// A wildcard name is not a name the API can look up, so it is resolved by listing and
			// matching like the other non-exact terms.
			listImageSelector = append(listImageSelector, imageSelector)
			continue
		}
		image, err := p.ox.ImageView(ctx, oxide.ImageViewParams{
			Image:   oxide.NameOrId(nameOrID),
			Project: oxide.NameOrId(project),
		})
		if err != nil {
			if errors.Is(err, oxide.ErrObjectNotFound) {
				continue
			}
			return nil, fmt.Errorf(
				"image %q from project %q: %w",
				nameOrID,
				imageSelector.Project,
				err,
			)
		}
		resolvedImages = append(resolvedImages, *image)
	}

	// No listImageSelector has an ID or an exact name, else it would have been looked up via a direct query (ImageView).
	// To efficiently make API calls, we now group by project.
	projectToSelectors := lo.GroupBy(
		listImageSelector,
		func(selector v1alpha1.ImageSelector) string { return selector.Project },
	)
	for project, selectors := range projectToSelectors {
		images, err := p.ox.ImageListAllPages(ctx, oxide.ImageListParams{
			Project: oxide.NameOrId(project),
		})
		if err != nil {
			return nil, fmt.Errorf("list error from project %q", project)
		}
		for _, selector := range selectors {
			for _, image := range images {
				if selector.Name != "" && !resolver.MatchName(selector.Name, string(image.Name)) {
					continue
				}
				if selector.OS != "" && !strings.EqualFold(selector.OS, image.Os) {
					continue
				}
				if selector.Version != "" &&
					!resolver.MatchVersion(selector.Version, image.Version) {
					continue
				}
				resolvedImages = append(resolvedImages, image)
			}
		}
	}
	resolvedImages = lo.Uniq(resolvedImages)

	// Sort by TimeCreate in descending order
	// Secondary sort is on ID which is guaranteed to be unique
	slices.SortStableFunc(resolvedImages, func(a, b oxide.Image) int {
		if b.TimeCreated.Compare(*a.TimeCreated) == 0 {
			return strings.Compare(b.Id, a.Id)
		}
		return b.TimeCreated.Compare(*a.TimeCreated)
	})

	return lo.Map(resolvedImages, func(image oxide.Image, _ int) v1alpha1.OxideImage {
		return v1alpha1.OxideImage{
			Project:     image.ProjectId,
			ID:          image.Id,
			Name:        string(image.Name),
			OS:          image.Os,
			Version:     image.Version,
			TimeCreated: metav1.NewTime(*image.TimeCreated),
		}
	}), nil
}

// DescribeSelectors renders image selectors back as the terms they were written with, so the
// ImagesResolved condition can say what was looked for rather than only that nothing was found.
func DescribeSelectors(selectors []v1alpha1.ImageSelector) string {
	if len(selectors) == 0 {
		return "no image selectors are configured"
	}
	described := lo.Map(selectors, func(selector v1alpha1.ImageSelector, _ int) string {
		terms := []string{}
		for _, term := range []struct{ key, value string }{
			{"id", selector.ID},
			{"name", selector.Name},
			{"os", selector.OS},
			{"version", selector.Version},
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
	return "no images matched " + strings.Join(described, " or ")
}

// DriftReason compares the image the instance booted from against the one the nodeclass would launch
// now, which is the first of the resolved images.
//
// Only the first: the runners-up are reported so a wildcard selector can be understood, not because a
// node is entitled to any of them. Nodes are consequently rolled onto a newly uploaded image that a
// wildcard matches, which is the point of writing a wildcard rather than pinning a version.
//
// An empty resolved list is not drift. It is a resolution that found nothing, already reported as
// ImagesResolved being false, and replacing the fleet over it would take every node down to launch
// replacements that cannot launch.
func (p *Provider) DriftReason(
	nodeClaim *karpv1.NodeClaim,
	nodeClass *v1alpha1.OxideNodeClass,
) karpcloudprovider.DriftReason {
	launched := nodeClaim.Status.ImageID
	desired := lo.FirstOrEmpty(nodeClass.Status.Images).ID
	if launched == "" || desired == "" {
		return ""
	}
	return lo.Ternary(launched != desired, providers.DriftReasonImage, "")
}
