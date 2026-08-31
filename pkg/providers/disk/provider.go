// Package disk manages Oxide disks independently of the workflows that own them.
package disk

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/oxidecomputer/oxide.go/oxide"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	defaultPollInterval         = 2 * time.Second
	defaultDeleteAttemptTimeout = 10 * time.Second

	// DefaultDeleteTimeout bounds retries for deleting one disk.
	DefaultDeleteTimeout = 90 * time.Second
)

// VerifyOwnership verifies that a disk is safe for a destructive operation.
type VerifyOwnership func(*oxide.Disk) error

// Provider performs common Oxide disk operations in one project.
type Provider struct {
	ox      *oxide.Client
	project string

	pollInterval         time.Duration
	deleteTimeout        time.Duration
	deleteAttemptTimeout time.Duration
}

// Option configures a Provider.
type Option func(*Provider)

// WithPollInterval replaces the default interval between state checks and delete attempts.
func WithPollInterval(interval time.Duration) Option {
	return func(p *Provider) { p.pollInterval = interval }
}

// WithDeleteTimeout replaces the default deadline for deleting one disk.
func WithDeleteTimeout(timeout time.Duration) Option {
	return func(p *Provider) { p.deleteTimeout = timeout }
}

// WithDeleteAttemptTimeout replaces the deadline for one disk view/delete attempt.
func WithDeleteAttemptTimeout(timeout time.Duration) Option {
	return func(p *Provider) { p.deleteAttemptTimeout = timeout }
}

// NewProvider constructs an Oxide disk provider.
func NewProvider(ox *oxide.Client, project string, opts ...Option) *Provider {
	p := &Provider{
		ox:                   ox,
		project:              project,
		pollInterval:         defaultPollInterval,
		deleteTimeout:        DefaultDeleteTimeout,
		deleteAttemptTimeout: defaultDeleteAttemptTimeout,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Create starts creating a disk in the provider's project.
func (p *Provider) Create(ctx context.Context, body *oxide.DiskCreate) (*oxide.Disk, error) {
	return p.ox.DiskCreate(ctx, oxide.DiskCreateParams{
		Project: oxide.NameOrId(p.project),
		Body:    body,
	})
}

// View returns a disk by name or immutable ID.
func (p *Provider) View(ctx context.Context, ref string) (*oxide.Disk, error) {
	return p.ox.DiskView(ctx, oxide.DiskViewParams{
		Disk:    oxide.NameOrId(ref),
		Project: p.projectFor(ref),
	})
}

// List returns every disk in the provider's project.
func (p *Provider) List(ctx context.Context) ([]oxide.Disk, error) {
	return p.ox.DiskListAllPages(ctx, oxide.DiskListParams{
		Project: oxide.NameOrId(p.project),
	})
}

// ListAttached returns every disk attached to an instance.
func (p *Provider) ListAttached(ctx context.Context, instanceID string) ([]oxide.Disk, error) {
	return p.ox.InstanceDiskListAllPages(ctx, oxide.InstanceDiskListParams{
		Instance: oxide.NameOrId(instanceID),
	})
}

// WaitDetached waits until disk is detached and returns its latest representation.
func (p *Provider) WaitDetached(ctx context.Context, disk *oxide.Disk) (*oxide.Disk, error) {
	return p.waitFor(ctx, disk, func(state oxide.DiskStateState) bool {
		return state == oxide.DiskStateStateDetached
	})
}

// WaitAttachable waits until disk can be attached or is already attached.
func (p *Provider) WaitAttachable(ctx context.Context, disk *oxide.Disk) (*oxide.Disk, error) {
	return p.waitFor(ctx, disk, isAttachable)
}

func (p *Provider) waitFor(
	ctx context.Context,
	disk *oxide.Disk,
	ready func(oxide.DiskStateState) bool,
) (*oxide.Disk, error) {
	current := disk
	if done, err := readyState(current, ready); done || err != nil {
		return current, err
	}
	err := wait.PollUntilContextCancel(
		ctx,
		p.pollInterval,
		false,
		func(ctx context.Context) (bool, error) {
			view, err := p.View(ctx, current.Id)
			if errors.Is(err, oxide.ErrObjectNotFound) {
				return false, fmt.Errorf("disk %s was deleted while waiting for it", disk.Name)
			}
			if err != nil {
				return false, fmt.Errorf("unable to view disk %s, %w", disk.Name, err)
			}
			current = view
			return readyState(current, ready)
		},
	)
	return current, err
}

func readyState(disk *oxide.Disk, ready func(oxide.DiskStateState) bool) (bool, error) {
	state := disk.State.State()
	if ready(state) {
		return true, nil
	}
	if state == oxide.DiskStateStateFaulted || state == oxide.DiskStateStateDestroyed {
		return false, fmt.Errorf("disk %s entered state %q", disk.Name, state)
	}
	return false, nil
}

// DeleteOwned deletes a disk once, after re-reading and verifying its ownership by immutable ID.
func (p *Provider) DeleteOwned(
	ctx context.Context,
	ref string,
	verify VerifyOwnership,
) error {
	disk, err := p.View(ctx, ref)
	if errors.Is(err, oxide.ErrObjectNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := verify(disk); err != nil {
		return err
	}
	err = p.ox.DiskDelete(ctx, oxide.DiskDeleteParams{Disk: oxide.NameOrId(disk.Id)})
	if errors.Is(err, oxide.ErrObjectNotFound) {
		return nil
	}
	return err
}

// DeleteOwnedEventually retries ownership-checked deletion until it succeeds or ctx ends.
func (p *Provider) DeleteOwnedEventually(
	ctx context.Context,
	ref string,
	verify VerifyOwnership,
) error {
	if ref == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, p.deleteTimeout)
	defer cancel()

	var lastErr error
	err := wait.PollUntilContextCancel(
		ctx,
		p.pollInterval,
		true,
		func(ctx context.Context) (bool, error) {
			attemptCtx, cancel := context.WithTimeout(ctx, p.deleteAttemptTimeout)
			defer cancel()

			disk, err := p.View(attemptCtx, ref)
			if errors.Is(err, oxide.ErrObjectNotFound) {
				return true, nil
			}
			if err != nil {
				lastErr = err
				return false, nil
			}
			if err := verify(disk); err != nil {
				return false, err
			}

			err = p.ox.DiskDelete(attemptCtx, oxide.DiskDeleteParams{
				Disk: oxide.NameOrId(disk.Id),
			})
			if err == nil || errors.Is(err, oxide.ErrObjectNotFound) {
				return true, nil
			}
			lastErr = err
			log.FromContext(ctx).V(1).Info(
				"disk deletion failed, retrying",
				"disk", ref,
				"reason", err.Error(),
			)
			return false, nil
		},
	)
	if err != nil {
		if wait.Interrupted(err) && lastErr != nil {
			err = lastErr
		}
		return fmt.Errorf("unable to delete disk %s, %w", ref, err)
	}
	return nil
}

func (p *Provider) projectFor(ref string) oxide.NameOrId {
	if _, err := uuid.Parse(ref); err == nil {
		return ""
	}
	return oxide.NameOrId(p.project)
}

func isAttachable(state oxide.DiskStateState) bool {
	switch state {
	case oxide.DiskStateStateDetached,
		oxide.DiskStateStateAttaching,
		oxide.DiskStateStateAttached:
		return true
	default:
		return false
	}
}
