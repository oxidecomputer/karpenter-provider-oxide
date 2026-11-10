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

// Package capacity publishes provider capacity.
package capacity

import (
	"context"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/storagerefusal"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	// GiB is how memory is reported here.
	GiB = 1 << 30

	heartbeat = 5 * time.Minute
)

// Controller publishes the capacity view.
type Controller struct {
	kubeClient    client.Client
	instanceTypes *instancetype.Provider
	storage       *storagerefusal.Tracker
	period        time.Duration

	crdMissingLogged bool
}

func NewController(
	kubeClient client.Client,
	instanceTypes *instancetype.Provider,
	storageRefusals *storagerefusal.Tracker,
	period time.Duration,
) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		instanceTypes: instanceTypes,
		storage:       storageRefusals,
		period:        period,
	}
}

func (c *Controller) Name() string {
	return "capacity"
}

// Reconcile renders the current state, publishes the metrics from it, and writes the object if it has changed.
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	reading := c.observe()
	status := c.render(reading)
	c.publishMetrics(reading)

	if err := c.publish(ctx, status); err != nil {
		c.logPublishFailure(ctx, err)
		return reconciler.Result{RequeueAfter: c.period}, nil
	}
	c.crdMissingLogged = false

	return reconciler.Result{RequeueAfter: c.period}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

func (c *Controller) publish(ctx context.Context, status v1alpha1.OxideCapacityStatus) error {
	if err := limitStatus(&status); err != nil {
		return err
	}
	existing := &v1alpha1.OxideCapacity{}
	err := c.kubeClient.Get(ctx, client.ObjectKey{Name: v1alpha1.OxideCapacityName}, existing)
	switch {
	case kerrors.IsNotFound(err):
		created := &v1alpha1.OxideCapacity{
			Name: v1alpha1.OxideCapacityName,
		}
		if err := c.kubeClient.Create(ctx, created); err != nil {
			return err
		}
		created.Status = status
		return c.kubeClient.Status().Update(ctx, created)
	case err != nil:
		return err
	}

	if unchanged(existing.Status, status) {
		return nil
	}
	existing.Status = status
	return c.kubeClient.Status().Update(ctx, existing)
}

func unchanged(published, current v1alpha1.OxideCapacityStatus) bool {
	if published.ObservedAt == nil || current.ObservedAt == nil {
		return false
	}
	if current.ObservedAt.Sub(published.ObservedAt.Time) >= heartbeat {
		return false
	}
	withoutTime := published
	withoutTime.ObservedAt = current.ObservedAt
	return equality.Semantic.DeepEqual(withoutTime, current)
}

func (c *Controller) logPublishFailure(ctx context.Context, err error) {
	if !crdMissing(err) {
		log.FromContext(ctx).V(1).Info(
			"unable to publish the capacity view, retrying next period",
			"reason", err.Error(),
		)
		return
	}
	if c.crdMissingLogged {
		log.FromContext(ctx).V(1).Info(
			"the OxideCapacity CRD is still not installed, capacity is reported by metrics only",
			"reason", err.Error(),
		)
		return
	}
	log.FromContext(ctx).Error(
		err,
		"the OxideCapacity CRD is not installed, so which instance shapes are unavailable is "+
			"reported by metrics only. Upgrade the karpenter-provider-oxide-crds chart to the same version as the "+
			"controller. Node provisioning is unaffected",
	)
	c.crdMissingLogged = true
}

func crdMissing(err error) bool {
	return meta.IsNoMatchError(err) ||
		runtime.IsNotRegisteredError(err) ||
		kerrors.IsNotFound(err)
}
