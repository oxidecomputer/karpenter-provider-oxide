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
//
// It exists separately from Controller so that the rack is walked on a fixed schedule rather than
// once per node: sled state and policy change without any node changing, so something has to
// re-read them, and letting per-node reconciles drive that would scale a fleet-wide walk with the
// size of the cluster. Controller only refreshes to resolve a node it cannot place at all.
type RefreshController struct {
	sledProvider *sled.Provider
	period       time.Duration
	// permissionErrorLogged records that the "your token cannot read the system API" error has already
	// been reported. It is the expected failure when inventory is enabled against a project-scoped
	// token, and it recurs every period, so it is said loudly once and quietly after that.
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
//
// It never returns an error. Inventory is opt-in observability that no other controller reads, so a
// system API that cannot be reached has to degrade to "no placement labels" rather than surface a
// failure: returning an error here would put the controller into exponential backoff and report an
// unhealthy condition for something the provider does not need in order to launch nodes.
func (c *RefreshController) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	if _, err := c.sledProvider.Refresh(ctx, true); err != nil {
		if c.permissionErrorLogged {
			log.FromContext(ctx).V(1).Info(
				"unable to refresh the sled inventory, retrying next period",
				"reason", err.Error(),
			)
		} else {
			// The first failure is the useful one: it is almost always the token lacking fleet
			// permissions, and an operator who enabled inventory needs to be told that plainly.
			log.FromContext(ctx).Error(
				err,
				"unable to refresh the sled inventory, sled placement labels will not be applied. "+
					"This requires an Oxide token with fleet-level read access to the system API. "+
					"Set settings.inventory.enabled=false to turn this off. Node provisioning is unaffected",
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
