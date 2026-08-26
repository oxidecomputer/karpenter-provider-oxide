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

// Package launch watches instance startup.
package launch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cloudprovider"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/instance"
	"github.com/oxidecomputer/oxide.go/oxide"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const resyncPeriod = time.Minute

// Controller deletes NodeClaims whose instance is not going to register.
type Controller struct {
	kubeClient client.Client
	ox         *oxide.Client
	deadlines  instance.Deadlines
	now        func() time.Time
}

func NewController(
	kubeClient client.Client,
	ox *oxide.Client,
	deadlines instance.Deadlines,
) *Controller {
	return &Controller{
		kubeClient: kubeClient,
		ox:         ox,
		deadlines:  deadlines,
		now:        time.Now,
	}
}

func (c *Controller) Name() string {
	return "nodeclaim-launch-watchdog"
}

// Reconcile decides whether one launched NodeClaim is still worth waiting for.
func (c *Controller) Reconcile(
	ctx context.Context,
	nodeClaim *karpv1.NodeClaim,
) (reconcile.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	if !nodeClaim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	if nodeClaim.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue() {
		return reconcile.Result{}, nil
	}

	instanceID, err := cloudprovider.InstanceIDFromProviderID(nodeClaim.Status.ProviderID)
	if err != nil {
		return reconcile.Result{}, nil
	}

	view, err := c.ox.InstanceView(
		ctx,
		oxide.InstanceViewParams{Instance: oxide.NameOrId(instanceID)},
	)
	if err != nil {
		if errors.Is(err, oxide.ErrObjectNotFound) {

			return reconcile.Result{RequeueAfter: resyncPeriod}, nil
		}
		return reconcile.Result{}, fmt.Errorf("unable to view instance %s, %w", instanceID, err)
	}

	elapsed := c.now().Sub(launchedAt(nodeClaim).Time)
	verdict, reason := c.judge(view.RunState, elapsed)
	switch verdict {
	case wait:
		return reconcile.Result{RequeueAfter: resyncPeriod}, nil
	case waitUntilDeadline:

		return reconcile.Result{RequeueAfter: c.deadlines.InstanceStarting - elapsed}, nil
	case undeletable:

		log.FromContext(ctx).V(1).Info(
			"leaving a NodeClaim whose instance is in a state the Oxide API will not delete",
			"instance-id", instanceID,
			"run-state", view.RunState,
			"launched-for", elapsed.Round(time.Second).String(),
			"reason", reason,
		)
		return reconcile.Result{RequeueAfter: resyncPeriod}, nil
	}

	log.FromContext(ctx).Info(
		"deleting a NodeClaim whose instance is not going to register",
		"instance-id", instanceID,
		"run-state", view.RunState,
		"launched-for", elapsed.Round(time.Second).String(),
		"reason", reason,
	)
	metrics.WatchdogDeletions.WithLabelValues(string(view.RunState)).Inc()
	if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(
			fmt.Errorf("unable to delete the NodeClaim, %w", err),
		)
	}
	return reconcile.Result{}, nil
}

type verdict int

const (
	wait verdict = iota

	waitUntilDeadline

	undeletable

	remove
)

func (c *Controller) judge(state oxide.InstanceState, elapsed time.Duration) (verdict, string) {
	switch state {
	case oxide.InstanceStateRunning:
		return wait, ""

	case oxide.InstanceStateFailed, oxide.InstanceStateDestroyed:
		return remove, "the instance reached a terminal state without registering"

	case oxide.InstanceStateCreating:
		return undeletable, "the instance is still creating although the launch reported it as placed"

	default:
		if elapsed >= c.deadlines.InstanceStarting {
			return remove, fmt.Sprintf(
				"the instance has been in state %q for longer than the launch deadline", state,
			)
		}
		return waitUntilDeadline, ""
	}
}

func launchedAt(nodeClaim *karpv1.NodeClaim) metav1.Time {
	if launched := nodeClaim.StatusConditions().
		Get(karpv1.ConditionTypeLaunched); launched != nil &&
		!launched.LastTransitionTime.IsZero() {
		return launched.LastTransitionTime
	}
	return nodeClaim.CreationTimestamp
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&karpv1.NodeClaim{}).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
