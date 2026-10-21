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
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project"
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
	ox       *oxide.Client
	projects *project.Resolver
	cache    *resolver.Cache[reading]
}

// reading is one resolution as cached: the images matched, and the selectors that found their image
// in a project other than the one they meant.
type reading struct {
	images    []v1alpha1.OxideImage
	misplaced []string
}

// NewProvider returns a provider that re-resolves a nodeclass's images once its reading is older than ttl.
func NewProvider(ox *oxide.Client, projects *project.Resolver, ttl time.Duration) *Provider {
	return &Provider{ox: ox, projects: projects, cache: resolver.NewCache[reading]("image", ttl)}
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
		func(ctx context.Context) (reading, error) {
			return p.resolve(ctx, selectors)
		},
	)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Resolved: slices.Clone(entry.Value.images),
		Age:      entry.Age(time.Now()),
		Stale:    entry.Stale,
	}

	switch {
	case len(entry.Value.misplaced) > 0:
		result.Reason = v1alpha1.ConditionReasonProjectMismatch
		result.Message = strings.Join(entry.Value.misplaced, "; ")
	case len(result.Resolved) == 0:
		result.Unmatched = selectors
		result.Reason = v1alpha1.ConditionReasonImagesNotFound
		result.Message = DescribeSelectors(selectors)
	}
	return result, nil
}

func (p *Provider) resolve(
	ctx context.Context,
	imageSelectors []v1alpha1.ImageSelector,
) (reading, error) {
	var out reading
	var resolvedImages []oxide.Image
	var listImageSelector []v1alpha1.ImageSelector

	for _, imageSelector := range imageSelectors {
		if imageSelector.ID == "" {
			listImageSelector = append(listImageSelector, imageSelector)
			continue
		}
		// The API refuses a project alongside an ID, so the project is checked after the fact.
		image, err := p.ox.ImageView(ctx, oxide.ImageViewParams{
			Image: oxide.NameOrId(imageSelector.ID),
		})
		if err != nil {
			if errors.Is(err, oxide.ErrObjectNotFound) {
				continue
			}
			return reading{}, fmt.Errorf("image %q: %w", imageSelector.ID, err)
		}
		if !matches(imageSelector, *image) {
			continue
		}
		inProject, err := p.inProject(ctx, imageSelector, *image)
		if err != nil {
			return reading{}, err
		}
		if !inProject {
			out.misplaced = append(out.misplaced, fmt.Sprintf(
				"image %s is in project %s, not project %s",
				image.Id, image.ProjectId, p.projects.For(imageSelector.Project),
			))
			continue
		}
		resolvedImages = append(resolvedImages, *image)
	}

	listed := map[string][]oxide.Image{}
	for _, selector := range listImageSelector {
		for _, in := range p.listProjects(selector) {
			images, ok := listed[in]
			if !ok {
				var err error
				images, err = p.ox.ImageListAllPages(ctx, oxide.ImageListParams{
					Project: oxide.NameOrId(in),
				})
				if err != nil {
					return reading{}, fmt.Errorf(
						"unable to list images in %s: %w", describeProject(in), err,
					)
				}
				listed[in] = images
			}
			for _, image := range images {
				if matches(selector, image) {
					resolvedImages = append(resolvedImages, image)
				}
			}
		}
	}
	resolvedImages = lo.UniqBy(resolvedImages, func(image oxide.Image) string { return image.Id })

	slices.SortStableFunc(resolvedImages, func(a, b oxide.Image) int {
		if b.TimeCreated.Compare(*a.TimeCreated) == 0 {
			return strings.Compare(b.Id, a.Id)
		}
		return b.TimeCreated.Compare(*a.TimeCreated)
	})

	out.images = lo.Map(resolvedImages, func(image oxide.Image, _ int) v1alpha1.OxideImage {
		return v1alpha1.OxideImage{
			Project:     image.ProjectId,
			ID:          image.Id,
			Name:        string(image.Name),
			OS:          image.Os,
			Version:     image.Version,
			SizeBytes:   int64(image.Size),
			TimeCreated: metav1.NewTime(*image.TimeCreated),
		}
	})
	return out, nil
}

// listProjects is where a selector without an ID looks: its own project, or the controller's
// project plus the silo images every project can boot from.
func (p *Provider) listProjects(selector v1alpha1.ImageSelector) []string {
	if selector.Project != "" {
		return []string{selector.Project}
	}
	return []string{"", p.projects.For("")}
}

// inProject reports whether an image found by ID is one the selector's project can use.
func (p *Provider) inProject(
	ctx context.Context,
	selector v1alpha1.ImageSelector,
	image oxide.Image,
) (bool, error) {
	if image.ProjectId == "" {
		return true, nil
	}
	return p.projects.Matches(ctx, p.projects.For(selector.Project), image.ProjectId)
}

func describeProject(in string) string {
	if in == "" {
		return "the silo"
	}
	return fmt.Sprintf("project %q", in)
}

// matches reports whether an image satisfies every term the selector sets other than its identity.
func matches(selector v1alpha1.ImageSelector, image oxide.Image) bool {
	if selector.Name != "" && !selector.Name.Matches(string(image.Name)) {
		return false
	}
	if selector.OS != "" && !strings.EqualFold(selector.OS, image.Os) {
		return false
	}
	if selector.Version != "" && !selector.Version.Matches(image.Version) {
		return false
	}
	return true
}

// DescribeSelectors renders image selectors back as the terms they were written with.
func DescribeSelectors(selectors []v1alpha1.ImageSelector) string {
	if len(selectors) == 0 {
		return "no image selectors are configured"
	}
	described := lo.Map(selectors, func(selector v1alpha1.ImageSelector, _ int) string {
		terms := []string{}
		for _, term := range []struct{ key, value string }{
			{"id", string(selector.ID)},
			{"name", string(selector.Name)},
			{"os", selector.OS},
			{"version", string(selector.Version)},
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
