// Package project answers which Oxide project a selector means and whether a resource is in it.
package project

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/resolver"
	"github.com/oxidecomputer/oxide.go/oxide"
)

// Resolver maps project references to project IDs, defaulting selectors that left the project out.
type Resolver struct {
	ox *oxide.Client

	defaultProject string
	ids            *resolver.Cache[string]
}

// NewResolver returns a resolver whose selectors fall back to defaultProject, the controller's own project.
func NewResolver(ox *oxide.Client, defaultProject string, ttl time.Duration) *Resolver {
	return &Resolver{
		ox:             ox,
		defaultProject: defaultProject,
		ids:            resolver.NewCache[string]("project", ttl),
	}
}

// Scope returns the parent selector, a project or a VPC, to send alongside refs: none when any
// ref is an ID, since the API refuses a parent with one, and parent otherwise.
func Scope(parent string, refs ...string) oxide.NameOrId {
	for _, ref := range refs {
		if _, err := uuid.Parse(ref); err == nil {
			return ""
		}
	}
	return oxide.NameOrId(parent)
}

// For returns the project a selector means: the one it wrote, or the controller's project.
func (r *Resolver) For(project string) string {
	if project != "" {
		return project
	}
	return r.defaultProject
}

// Matches reports whether projectID is the project that ref, a project name or ID, refers to.
func (r *Resolver) Matches(ctx context.Context, ref, projectID string) (bool, error) {
	if _, err := uuid.Parse(ref); err == nil {
		return ref == projectID, nil
	}
	entry, err := r.ids.Get(ctx, ref, func(ctx context.Context) (string, error) {
		project, err := r.ox.ProjectView(ctx, oxide.ProjectViewParams{Project: oxide.NameOrId(ref)})
		if err != nil {
			return "", fmt.Errorf("project %q: %w", ref, err)
		}
		return project.Id, nil
	})
	if err != nil {
		return false, err
	}
	return entry.Value == projectID, nil
}
