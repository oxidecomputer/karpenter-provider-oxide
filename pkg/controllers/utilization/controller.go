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

// Package utilization keeps the silo's quota snapshot fresh.
//
// The quota moves for reasons Karpenter never sees: anything else in the silo starting or stopping
// changes what is provisioned, and an administrator can change the capacity outright. So it is polled
// rather than invalidated, and polled on its own schedule rather than off the back of node activity,
// which would leave a cluster that has stopped scaling with a snapshot that never updates again.
package utilization

import (
	"context"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Controller refreshes the silo quota on a fixed interval.
type Controller struct {
	quotas *utilization.Provider
	period time.Duration
	// failureLogged records that the read has already been reported as failing, so a persistent
	// permissions problem is said once at error rather than every period forever.
	failureLogged bool
}

func NewController(quotas *utilization.Provider, period time.Duration) *Controller {
	return &Controller{
		quotas: quotas,
		period: period,
	}
}

func (c *Controller) Name() string {
	return "utilization"
}

// Reconcile re-reads the quota and schedules the next read.
//
// It never returns an error. The quota is a constraint that makes provisioning smarter, not one it
// depends on: an unreadable quota means shapes stop being filtered by it, which is the same behavior
// as before there was a quota check at all. Returning an error would instead put this into backoff
// and report the provider unhealthy for something nodes do not need.
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	if _, err := c.quotas.Refresh(ctx, true); err != nil {
		if c.failureLogged {
			log.FromContext(ctx).V(1).Info(
				"unable to read the silo's utilization, retrying next period",
				"reason", err.Error(),
			)
		} else {
			log.FromContext(ctx).Error(
				err,
				"unable to read the silo's utilization, so instance types will not be constrained by "+
					"the silo quota and a launch that exceeds it will fail at the API instead. "+
					"Node provisioning is otherwise unaffected",
			)
			c.failureLogged = true
		}
		return reconciler.Result{RequeueAfter: c.period}, nil
	}
	c.failureLogged = false

	return reconciler.Result{RequeueAfter: c.period}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
