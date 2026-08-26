// Package image resolves image selectors and drift.
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

// NewProvider returns a provider that re-resolves a nodeclass's images once its reading is older than ttl.
func NewProvider(ox *oxide.Client, ttl time.Duration) *Provider {
	return &Provider{ox: ox, cache: resolver.NewCache[[]v1alpha1.OxideImage]("image", ttl)}
}

// Resolve returns the images the nodeclass's selectors match, newest first.
func (p *Provider) Resolve(
	ctx context.Context,
	nodeClass *v1alpha1.OxideNodeClass,
) (Result, error) {
	return p.ResolveSelectors(ctx, nodeClass.Spec.ImageSelector)
}

// ResolveSelectors is Resolve for a selector list that is not the nodeclass's boot image selector.
func (p *Provider) ResolveSelectors(
	ctx context.Context,
	selectors []v1alpha1.ImageSelector,
) (Result, error) {
	if len(selectors) == 0 {
		return Result{}, nil
	}
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
	imageSelectors []v1alpha1.ImageSelector,
) ([]v1alpha1.OxideImage, error) {
	var resolvedImages []oxide.Image
	var listImageSelector []v1alpha1.ImageSelector

	for _, imageSelector := range imageSelectors {
		nameOrID := ""
		project := imageSelector.Project
		if imageSelector.ID != "" {
			nameOrID = imageSelector.ID

			project = ""
		} else if imageSelector.Name != "" && !strings.Contains(imageSelector.Name, "*") {
			nameOrID = imageSelector.Name
		} else {

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
			SizeBytes:   int64(image.Size),
			TimeCreated: metav1.NewTime(*image.TimeCreated),
		}
	}), nil
}

// DescribeSelectors renders image selectors back as the terms they were written with.
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

// DriftReason compares the image the instance booted from against the one the nodeclass would launch now.
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
