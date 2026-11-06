package framework

import (
	"context"
	"fmt"

	"github.com/oxidecomputer/oxide.go/oxide"
	"k8s.io/apimachinery/pkg/util/wait"
)

// DeleteInstance takes an instance away behind Karpenter's back, the way a console user
// would: stop, wait for Stopped, delete. Nexus detaches the disks and leaves them behind.
func (e *Env) DeleteInstance(ctx context.Context, id string) error {
	if _, err := e.Oxide.InstanceStop(ctx, oxide.InstanceStopParams{
		Instance: oxide.NameOrId(id),
	}); err != nil {
		return fmt.Errorf("stopping instance %s: %w", id, err)
	}
	var state oxide.InstanceState
	err := wait.PollUntilContextTimeout(ctx, e.Poll, e.DeleteTimeout, true,
		func(ctx context.Context) (bool, error) {
			view, err := e.Oxide.InstanceView(ctx, oxide.InstanceViewParams{
				Instance: oxide.NameOrId(id),
			})
			if err != nil {
				return false, err
			}
			state = view.RunState
			return state == oxide.InstanceStateStopped, nil
		})
	if err != nil {
		return fmt.Errorf("instance %s is %s after %s: %w", id, state, e.DeleteTimeout, err)
	}
	if err := e.Oxide.InstanceDelete(ctx, oxide.InstanceDeleteParams{
		Instance: oxide.NameOrId(id),
	}); err != nil {
		return fmt.Errorf("deleting instance %s: %w", id, err)
	}
	return nil
}
