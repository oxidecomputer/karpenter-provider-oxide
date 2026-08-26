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

package inventory

import (
	"context"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/sled"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// RefreshController keeps the sled inventory snapshot from going stale.
type RefreshController struct {
	sledProvider *sled.Provider
	period       time.Duration

	permissionErrorLogged bool
}

func NewRefreshController(sledProvider *sled.Provider, period time.Duration) *RefreshController {
	return &RefreshController{
		sledProvider: sledProvider,
		period:       period,
	}
}

func (c *RefreshController) Name() string {
	return "inventory.refresh"
}

// Reconcile refreshes the snapshot and schedules the next refresh.
func (c *RefreshController) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	if _, err := c.sledProvider.Refresh(ctx, true); err != nil {
		if c.permissionErrorLogged {
			log.FromContext(ctx).V(1).Info(
				"unable to refresh the sled inventory, retrying next period",
				"reason", err.Error(),
			)
		} else {

			log.FromContext(ctx).Error(
				err,
				"unable to refresh the sled inventory, sled placement labels will not be applied "+
					"and instance types will not be narrowed to what the rack has room for. "+
					"This requires an Oxide token with fleet-level read access to the system API. "+
					"Set settings.inventory.enabled=false to turn this off. Nodes still launch: "+
					"shapes the rack cannot place are learned from refused launches instead",
			)
			c.permissionErrorLogged = true
		}
		return reconciler.Result{RequeueAfter: c.period}, nil
	}
	c.permissionErrorLogged = false

	return reconciler.Result{RequeueAfter: c.period}, nil
}

func (c *RefreshController) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
