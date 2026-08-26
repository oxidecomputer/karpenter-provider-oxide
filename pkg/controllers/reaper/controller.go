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

// Package reaper removes the Oxide resources a launch left behind.
package reaper

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/instance"
	"github.com/oxidecomputer/oxide.go/oxide"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Controller collects the Oxide resources of NodeClaims that no longer exist.
type Controller struct {
	kubeClient client.Client

	apiReader  client.Reader
	ox         *oxide.Client
	project    string
	clusterUID types.UID

	instances *instance.Provider

	period     time.Duration
	quarantine time.Duration

	firstSeen map[string]time.Time

	now func() time.Time
}

type resource struct {
	kind   string
	id     string
	name   string
	marker ownership.Marker

	state string

	attachedTo string
}

func (r resource) undeletable() bool {
	switch r.kind {
	case instance.KindInstance:
		return r.state == string(oxide.InstanceStateCreating)
	case instance.KindDisk:
		return r.state == string(oxide.DiskStateStateCreating)
	default:
		return false
	}
}

func (r resource) key() string { return r.kind + "/" + r.id }

func NewController(
	kubeClient client.Client,
	apiReader client.Reader,
	ox *oxide.Client,
	project string,
	clusterUID types.UID,
	instances *instance.Provider,
	period time.Duration,
	quarantine time.Duration,
) *Controller {
	return &Controller{
		kubeClient: kubeClient,
		apiReader:  apiReader,
		ox:         ox,
		project:    project,
		clusterUID: clusterUID,
		instances:  instances,
		period:     period,
		quarantine: quarantine,
		firstSeen:  map[string]time.Time{},
		now:        time.Now,
	}
}

func (c *Controller) Name() string {
	return "orphan-reaper"
}

// Reconcile scans the project once and deletes what it can prove is an orphan.
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	owned, err := c.owned(ctx)
	if err != nil {
		log.FromContext(ctx).Error(
			err,
			"unable to list this cluster's Oxide resources, so nothing is collected this period",
		)
		return reconciler.Result{RequeueAfter: c.period}, nil
	}

	live, err := c.liveNodeClaimUIDs(ctx)
	if err != nil {

		log.FromContext(ctx).Error(
			err,
			"unable to list NodeClaims, so nothing is collected this period",
		)
		return reconciler.Result{RequeueAfter: c.period}, nil
	}

	orphans := c.quarantined(owned, live)
	c.publish(orphans)

	collected := sets.New[string]()
	for _, orphan := range orphans {
		if orphan.kind != instance.KindInstance {
			continue
		}
		if c.collect(ctx, orphan) {
			collected.Insert(orphan.id)
		}
	}
	for _, orphan := range orphans {
		if orphan.kind != instance.KindDisk {
			continue
		}

		if orphan.attachedTo != "" && !collected.Has(orphan.attachedTo) {
			log.FromContext(ctx).V(1).Info(
				"leaving an orphaned disk that is still attached to an instance",
				"disk", orphan.name,
				"disk-id", orphan.id,
				"instance-id", orphan.attachedTo,
			)
			continue
		}
		c.collect(ctx, orphan)
	}

	return reconciler.Result{RequeueAfter: c.period}, nil
}

func (c *Controller) owned(ctx context.Context) ([]resource, error) {
	instances, err := c.ox.InstanceListAllPages(ctx, oxide.InstanceListParams{
		Project: oxide.NameOrId(c.project),
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list the project's instances, %w", err)
	}
	disks, err := c.ox.DiskListAllPages(ctx, oxide.DiskListParams{
		Project: oxide.NameOrId(c.project),
	})
	if err != nil {
		return nil, fmt.Errorf("unable to list the project's disks, %w", err)
	}

	owned := make([]resource, 0, len(instances)+len(disks))
	for _, listed := range instances {
		marker, ok := ownership.Parse(listed.Description)
		if !ok || marker.ClusterUID != c.clusterUID {
			continue
		}
		owned = append(owned, resource{
			kind:   instance.KindInstance,
			id:     listed.Id,
			name:   string(listed.Name),
			marker: marker,
			state:  string(listed.RunState),
		})
	}
	for _, listed := range disks {
		marker, ok := ownership.Parse(listed.Description)
		if !ok || marker.ClusterUID != c.clusterUID {
			continue
		}
		owned = append(owned, resource{
			kind:       instance.KindDisk,
			id:         listed.Id,
			name:       string(listed.Name),
			marker:     marker,
			state:      string(listed.State.State()),
			attachedTo: attachedInstance(listed),
		})
	}
	return owned, nil
}

func (c *Controller) liveNodeClaimUIDs(ctx context.Context) (sets.Set[types.UID], error) {
	nodeClaims := &karpv1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, nodeClaims); err != nil {
		return nil, err
	}
	live := sets.New[types.UID]()
	for i := range nodeClaims.Items {
		live.Insert(nodeClaims.Items[i].UID)
	}
	return live, nil
}

func (c *Controller) quarantined(owned []resource, live sets.Set[types.UID]) []resource {
	now := c.now()
	seen := make(map[string]time.Time, len(c.firstSeen))

	var orphans []resource
	for _, candidate := range owned {
		if live.Has(candidate.marker.NodeClaimUID) {
			continue
		}
		key := candidate.key()
		since, known := c.firstSeen[key]
		if !known {
			since = now
		}
		seen[key] = since
		if now.Sub(since) >= c.quarantine {
			orphans = append(orphans, candidate)
		}
	}

	c.firstSeen = seen

	sort.SliceStable(orphans, func(i, j int) bool {
		return seen[orphans[i].key()].Before(seen[orphans[j].key()])
	})
	return orphans
}

func (c *Controller) publish(orphans []resource) {
	counts := map[string]int{instance.KindInstance: 0, instance.KindDisk: 0}
	for _, orphan := range orphans {
		counts[orphan.kind]++
	}
	for kind, count := range counts {
		metrics.OrphanResources.WithLabelValues(kind).Set(float64(count))
	}

	oldest := time.Duration(0)
	now := c.now()
	for _, since := range c.firstSeen {
		if age := now.Sub(since); age > oldest {
			oldest = age
		}
	}
	metrics.OrphanAgeSeconds.Set(metrics.Seconds(oldest))
}

func (c *Controller) collect(ctx context.Context, orphan resource) bool {
	if orphan.undeletable() {
		log.FromContext(ctx).V(1).Info(
			"leaving an orphan in a state the Oxide API will not delete it from",
			"kind", orphan.kind,
			"resource", orphan.name,
			"resource-id", orphan.id,
			"state", orphan.state,
		)
		return false
	}

	confirmed, err := c.confirm(ctx, orphan)
	if err != nil {
		log.FromContext(ctx).V(1).Info(
			"unable to confirm that a resource is orphaned, leaving it for the next pass",
			"kind", orphan.kind,
			"resource", orphan.name,
			"reason", err.Error(),
		)
		return false
	}
	if !confirmed {

		delete(c.firstSeen, orphan.key())
		return false
	}

	metrics.ReaperDeleteAttempts.WithLabelValues(orphan.kind).Inc()
	if err := c.delete(ctx, orphan); err != nil {
		metrics.ReaperDeleteFailures.WithLabelValues(orphan.kind).Inc()

		log.FromContext(ctx).Info(
			"unable to delete an orphaned Oxide resource, retrying next period",
			"kind", orphan.kind,
			"resource", orphan.name,
			"resource-id", orphan.id,
			"nodeclaim", orphan.marker.NodeClaimName,
			"reason", err.Error(),
		)
		return false
	}

	log.FromContext(ctx).Info(
		"deleted an Oxide resource whose NodeClaim no longer exists",
		"kind", orphan.kind,
		"resource", orphan.name,
		"resource-id", orphan.id,
		"nodeclaim", orphan.marker.NodeClaimName,
		"nodeclaim-uid", orphan.marker.NodeClaimUID,
	)
	delete(c.firstSeen, orphan.key())
	return true
}

func (c *Controller) confirm(ctx context.Context, orphan resource) (bool, error) {
	nodeClaim := &karpv1.NodeClaim{}
	err := c.apiReader.Get(ctx, client.ObjectKey{Name: orphan.marker.NodeClaimName}, nodeClaim)
	switch {
	case kerrors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf(
			"unable to read NodeClaim %s, %w", orphan.marker.NodeClaimName, err,
		)
	}
	return nodeClaim.UID != orphan.marker.NodeClaimUID, nil
}

func (c *Controller) delete(ctx context.Context, orphan resource) error {
	if orphan.kind == instance.KindInstance {
		err := c.instances.Delete(ctx, orphan.id, instance.Teardown{})
		if err == nil || errors.Is(err, instance.ErrNotFound) {
			return nil
		}
		return err
	}
	return c.instances.DeleteDisks(ctx, []string{orphan.id})
}

func attachedInstance(disk oxide.Disk) string {
	if attached, ok := disk.State.AsAttached(); ok {
		return attached.Instance
	}
	return ""
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
