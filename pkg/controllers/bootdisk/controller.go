package bootdisk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/metrics"
	bootdiskprovider "github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/bootdisk"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	"github.com/oxidecomputer/oxide.go/oxide"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const period = 5 * time.Second

// claimGrace covers the gap between claiming a disk and the instance create
// that attaches it, during which the disk still looks detached and idle.
const claimGrace = 5 * time.Minute

type Controller struct {
	kubeClient     client.Client
	reader         client.Reader
	pool           *bootdiskprovider.Provider
	quotas         *utilization.Provider
	lastOrphanScan time.Time
}

type poolState struct {
	nodeClasses    []v1alpha1.OxideNodeClass
	records        *v1alpha1.OxideBootDiskList
	liveClaims     map[types.UID]struct{}
	active         map[string]bool
	classByUID     map[types.UID]*v1alpha1.OxideNodeClass
	configByUID    map[types.UID]bootdiskprovider.Config
	desiredByUID   map[types.UID]int32
	recordsByClass map[types.UID][]*v1alpha1.OxideBootDisk
}

func NewController(
	kubeClient client.Client,
	reader client.Reader,
	pool *bootdiskprovider.Provider,
	quotas *utilization.Provider,
) *Controller {
	return &Controller{kubeClient: kubeClient, reader: reader, pool: pool, quotas: quotas}
}

func (c *Controller) Name() string { return "boot-disk-pool" }

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("controller", c.Name()))

	state, err := c.readState(ctx)
	if err != nil {
		return reconciler.Result{RequeueAfter: period}, err
	}
	c.scanOrphans(ctx, state.records)
	c.retireOld(state.recordsByClass, state.configByUID, state.desiredByUID)
	if err := c.createRecords(ctx, state); err != nil {
		return reconciler.Result{RequeueAfter: period}, err
	}

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for i := range state.records.Items {
		record := state.records.Items[i].DeepCopy()
		group.Go(func() error {
			if err := c.reconcileRecord(groupCtx, record, state.liveClaims); err != nil {
				log.FromContext(groupCtx).V(1).Info(
					"boot disk reconcile failed",
					"disk", record.Name,
					"reason", err.Error(),
				)
			}
			return nil
		})
	}
	_ = group.Wait()

	if err := c.publish(
		ctx,
		state.classByUID,
		state.configByUID,
		state.desiredByUID,
	); err != nil {
		return reconciler.Result{RequeueAfter: period}, err
	}
	return reconciler.Result{RequeueAfter: period}, nil
}

func (c *Controller) readState(ctx context.Context) (*poolState, error) {
	nodeClasses := &v1alpha1.OxideNodeClassList{}
	nodePools := &karpv1.NodePoolList{}
	nodeClaims := &karpv1.NodeClaimList{}
	records := &v1alpha1.OxideBootDiskList{}
	for _, list := range []client.ObjectList{nodeClasses, nodePools, nodeClaims, records} {
		if err := c.kubeClient.List(ctx, list); err != nil {
			return nil, err
		}
	}

	liveClaims := map[types.UID]struct{}{}
	population := map[string]int64{}
	poolClaims := map[string]int64{}
	for i := range nodeClaims.Items {
		claim := &nodeClaims.Items[i]
		if !claim.DeletionTimestamp.IsZero() || claim.Spec.NodeClassRef == nil {
			continue
		}
		liveClaims[claim.UID] = struct{}{}
		if oxideRef(claim.Spec.NodeClassRef) {
			population[claim.Spec.NodeClassRef.Name]++
		}
		poolClaims[claim.Labels[karpv1.NodePoolLabelKey]]++
	}

	active := map[string]bool{}
	for i := range nodePools.Items {
		nodePool := &nodePools.Items[i]
		ref := nodePool.Spec.Template.Spec.NodeClassRef
		if !nodePool.DeletionTimestamp.IsZero() || ref == nil || !oxideRef(ref) {
			continue
		}
		active[ref.Name] = true
		if nodePool.Spec.Replicas != nil && *nodePool.Spec.Replicas > poolClaims[nodePool.Name] {
			population[ref.Name] += *nodePool.Spec.Replicas - poolClaims[nodePool.Name]
		}
	}

	classByUID := map[types.UID]*v1alpha1.OxideNodeClass{}
	configByUID := map[types.UID]bootdiskprovider.Config{}
	desiredByUID := map[types.UID]int32{}
	for i := range nodeClasses.Items {
		nodeClass := &nodeClasses.Items[i]
		classByUID[nodeClass.UID] = nodeClass
		config, ok := bootdiskprovider.ConfigFor(nodeClass)
		if ok {
			configByUID[nodeClass.UID] = config
		}
		activeClass := active[nodeClass.Name] || population[nodeClass.Name] > 0
		if ok && nodeClass.DeletionTimestamp.IsZero() &&
			nodeClass.Spec.Storage.Boot.PoolEnabled() && activeClass {
			desiredByUID[nodeClass.UID] = target(
				nodeClass.Spec.Storage.Boot,
				population[nodeClass.Name],
			)
		}
	}

	recordsByClass := map[types.UID][]*v1alpha1.OxideBootDisk{}
	for i := range records.Items {
		record := &records.Items[i]
		uid := record.Spec.NodeClassRef.UID
		recordsByClass[uid] = append(recordsByClass[uid], record)
	}

	return &poolState{
		nodeClasses:    nodeClasses.Items,
		records:        records,
		liveClaims:     liveClaims,
		active:         active,
		classByUID:     classByUID,
		configByUID:    configByUID,
		desiredByUID:   desiredByUID,
		recordsByClass: recordsByClass,
	}, nil
}

func (c *Controller) scanOrphans(ctx context.Context, records *v1alpha1.OxideBootDiskList) {
	if time.Since(c.lastOrphanScan) < time.Minute {
		return
	}
	known := map[types.UID]struct{}{}
	for i := range records.Items {
		known[records.Items[i].UID] = struct{}{}
	}
	if err := c.pool.CollectOrphans(ctx, known); err != nil {
		log.FromContext(ctx).V(1).Info("boot disk orphan scan failed", "reason", err.Error())
		return
	}
	c.lastOrphanScan = time.Now()
}

func (c *Controller) createRecords(ctx context.Context, state *poolState) error {
	budget := c.creationBudget(state.nodeClasses, state.active)
	for uid, desired := range state.desiredByUID {
		config := state.configByUID[uid]
		class := state.classByUID[uid]
		current := int32(0)
		for _, record := range state.recordsByClass[uid] {
			if record.DeletionTimestamp.IsZero() && !record.Spec.Retire &&
				record.Spec.ClaimRef == nil && record.Spec.ConfigHash == config.Hash {
				current++
			}
		}
		for current < desired && budget >= config.SizeBytes {
			record := bootdiskprovider.NewRecord(config, class)
			if err := c.kubeClient.Create(ctx, record); err != nil {
				return err
			}
			state.records.Items = append(state.records.Items, *record)
			index := len(state.records.Items) - 1
			state.recordsByClass[uid] = append(
				state.recordsByClass[uid],
				&state.records.Items[index],
			)
			current++
			budget -= config.SizeBytes
		}
	}
	return nil
}

func target(boot v1alpha1.BootDisk, nodes int64) int32 {
	ready := int32((nodes*int64(boot.PoolPercentage()) + 99) / 100)
	ready = max(ready, boot.PoolMinReady())
	return min(ready, boot.PoolMaxReady())
}

func oxideRef(ref *karpv1.NodeClassReference) bool {
	return ref.Group == apis.Group && ref.Kind == "OxideNodeClass"
}

func (c *Controller) retireOld(
	records map[types.UID][]*v1alpha1.OxideBootDisk,
	configs map[types.UID]bootdiskprovider.Config,
	desired map[types.UID]int32,
) {
	for uid, items := range records {
		config, current := configs[uid]
		var available []*v1alpha1.OxideBootDisk
		for _, record := range items {
			if record.Spec.ClaimRef != nil ||
				record.Spec.Retire ||
				!record.DeletionTimestamp.IsZero() {
				continue
			}
			if !current || record.Spec.ConfigHash != config.Hash {
				record.Spec.Retire = true
				continue
			}
			available = append(available, record)
		}
		slices.SortFunc(available, func(a, b *v1alpha1.OxideBootDisk) int {
			aReady := a.Status.State == v1alpha1.BootDiskStateReady
			bReady := b.Status.State == v1alpha1.BootDiskStateReady
			if aReady != bReady {
				if aReady {
					return -1
				}
				return 1
			}
			return b.CreationTimestamp.Compare(a.CreationTimestamp.Time)
		})
		for i := desired[uid]; i < int32(len(available)); i++ {
			available[i].Spec.Retire = true
		}
	}
}

func (c *Controller) reconcileRecord(
	ctx context.Context,
	record *v1alpha1.OxideBootDisk,
	liveClaims map[types.UID]struct{},
) error {
	if record.Spec.Retire && record.DeletionTimestamp.IsZero() {
		current := &v1alpha1.OxideBootDisk{}
		if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: record.Name}, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !current.Spec.Retire {
			if current.Spec.ClaimRef != nil {
				return nil
			}
			stored := current.DeepCopy()
			current.Spec.Retire = true
			if err := c.kubeClient.Patch(ctx, current, client.MergeFromWithOptions(
				stored, client.MergeFromWithOptimisticLock{},
			)); err != nil {
				return err
			}
		}
		record = current
	}

	if record.Spec.ClaimRef != nil {
		if record.DeletionTimestamp.IsZero() && !record.Spec.Retire {
			live, err := c.claimLive(ctx, record.Spec.ClaimRef, liveClaims)
			if err != nil {
				return err
			}
			if live {
				return nil
			}
		}
		if within(record.Spec.ClaimedAt, claimGrace) {
			return nil
		}
		disk, err := c.pool.View(ctx, record)
		if errors.Is(err, oxide.ErrObjectNotFound) {
			return c.removeRecord(ctx, record)
		}
		if err != nil {
			return err
		}
		if attached(disk.State.State()) {
			return nil
		}
		record.Spec.Retire = true
	}

	if record.Spec.Retire || !record.DeletionTimestamp.IsZero() {
		disk, err := c.pool.View(ctx, record)
		if err == nil && attached(disk.State.State()) {
			return nil
		}
		if err != nil && !errors.Is(err, oxide.ErrObjectNotFound) {
			return err
		}
		if err := c.pool.Delete(ctx, record); err != nil {
			metrics.BootDiskPoolDeleteFailures.Inc()
			return err
		}
		return c.removeRecord(ctx, record)
	}

	if record.Status.State == v1alpha1.BootDiskStateReady {
		return nil
	}
	started := time.Now()
	disk, err := c.pool.Ensure(ctx, record)
	metrics.BootDiskPoolCreationSeconds.Observe(time.Since(started).Seconds())
	if err != nil {
		metrics.BootDiskPoolCreateFailures.Inc()
		return c.patchStatus(ctx, record, "", v1alpha1.BootDiskStatePending, err.Error())
	}
	return c.patchStatus(ctx, record, disk.Id, v1alpha1.BootDiskStateReady, "")
}

// claimLive confirms a claim's absence against the API server: the cached
// NodeClaim list can lag a launch that is already attaching this disk.
func (c *Controller) claimLive(
	ctx context.Context,
	ref *v1alpha1.BootDiskOwnerRef,
	cached map[types.UID]struct{},
) (bool, error) {
	if _, live := cached[ref.UID]; live {
		return true, nil
	}
	claim := &karpv1.NodeClaim{}
	if err := c.reader.Get(ctx, client.ObjectKey{Name: ref.Name}, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return claim.UID == ref.UID && claim.DeletionTimestamp.IsZero(), nil
}

func within(stamp *metav1.Time, window time.Duration) bool {
	return stamp != nil && time.Since(stamp.Time) < window
}

func (c *Controller) patchStatus(
	ctx context.Context,
	record *v1alpha1.OxideBootDisk,
	id string,
	state string,
	message string,
) error {
	current := &v1alpha1.OxideBootDisk{}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: record.Name}, current); err != nil {
		return client.IgnoreNotFound(err)
	}
	stored := current.DeepCopy()
	current.Status.ID = id
	current.Status.State = state
	current.Status.Message = message
	if equality.Semantic.DeepEqual(stored.Status, current.Status) {
		return nil
	}
	return c.kubeClient.Status().Patch(ctx, current, client.MergeFrom(stored))
}

func attached(state oxide.DiskStateState) bool {
	return state == oxide.DiskStateStateAttaching || state == oxide.DiskStateStateAttached
}

func (c *Controller) removeRecord(ctx context.Context, record *v1alpha1.OxideBootDisk) error {
	current := &v1alpha1.OxideBootDisk{}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: record.Name}, current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if controllerutil.ContainsFinalizer(current, v1alpha1.BootDiskFinalizer) {
		stored := current.DeepCopy()
		controllerutil.RemoveFinalizer(current, v1alpha1.BootDiskFinalizer)
		if err := c.kubeClient.Patch(ctx, current, client.MergeFrom(stored)); err != nil {
			return err
		}
	}
	if current.DeletionTimestamp.IsZero() {
		return client.IgnoreNotFound(c.kubeClient.Delete(ctx, current))
	}
	return nil
}

func (c *Controller) creationBudget(
	classes []v1alpha1.OxideNodeClass,
	active map[string]bool,
) int64 {
	if c.quotas == nil {
		return math.MaxInt64
	}
	quota, err := c.quotas.Quota()
	if err != nil {
		return math.MaxInt64
	}
	floor := int64(quota.CapacityStorage / 20)
	for i := range classes {
		if active[classes[i].Name] {
			floor = max(floor, distributedBytes(classes[i].Spec.Storage))
		}
	}
	return max(int64(0), quota.Remaining(utilization.ResourceStorage)-floor)
}

func distributedBytes(storage v1alpha1.Storage) int64 {
	total := storage.Boot.Size.Value()
	for _, set := range storage.DiskSets {
		if set.EffectiveType() == v1alpha1.DiskSetTypeDistributed {
			total += set.Size.Value() * int64(set.EffectiveCount())
		}
	}
	return total
}

func (c *Controller) publish(
	ctx context.Context,
	classes map[types.UID]*v1alpha1.OxideNodeClass,
	configs map[types.UID]bootdiskprovider.Config,
	desired map[types.UID]int32,
) error {
	records := &v1alpha1.OxideBootDiskList{}
	if err := c.kubeClient.List(ctx, records); err != nil {
		return err
	}
	metrics.BootDiskPoolDisks.Reset()
	for uid, nodeClass := range classes {
		status := v1alpha1.BootDiskPoolStatus{Desired: desired[uid]}
		config, configured := configs[uid]
		if configured {
			status.ConfigHash = config.Hash
		}
		for i := range records.Items {
			record := &records.Items[i]
			if record.Spec.NodeClassRef.UID != uid {
				continue
			}
			switch {
			case record.Spec.ClaimRef != nil:
				status.Claimed++
			case record.Spec.Retire || !configured || record.Spec.ConfigHash != config.Hash:
				status.Stale++
			case record.Status.State == v1alpha1.BootDiskStateReady && !record.Spec.Retire:
				status.Ready++
			default:
				status.Creating++
			}
			if status.Message == "" && record.Status.Message != "" {
				status.Message = record.Status.Message
			}
		}
		current := &v1alpha1.OxideNodeClass{}
		if err := c.kubeClient.Get(
			ctx,
			client.ObjectKey{Name: nodeClass.Name},
			current,
		); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if current.Status.Storage == nil {
			continue
		}
		stored := current.DeepCopy()
		current.Status.Storage.BootDiskPool = &status
		if !equality.Semantic.DeepEqual(
			stored.Status.Storage.BootDiskPool,
			current.Status.Storage.BootDiskPool,
		) {
			err := c.kubeClient.Status().Patch(ctx, current, client.MergeFrom(stored))
			if err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
				return fmt.Errorf("publish boot disk pool for %s: %w", current.Name, err)
			}
		}
		for state, count := range map[string]int32{
			"desired":  status.Desired,
			"ready":    status.Ready,
			"creating": status.Creating,
			"claimed":  status.Claimed,
			"stale":    status.Stale,
		} {
			metrics.BootDiskPoolDisks.WithLabelValues(current.Name, state).Set(float64(count))
		}
	}
	return nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
