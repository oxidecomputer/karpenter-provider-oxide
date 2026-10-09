package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/availability"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/kubeadm"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap/rke2"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cloudprovider"
	affinitycontroller "github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/affinity"
	bootdiskcontroller "github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/bootdisk"
	capacitycontroller "github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/inventory"
	launchcontroller "github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/launch"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/nodeclass"
	reapercontroller "github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/reaper"
	utilizationcontroller "github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/utilization"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/affinitygroup"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/bootdisk"
	diskprovider "github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/disk"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/image"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/instance"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/ippool"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/snapshot"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/sshkey"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/subnet"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/sled"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/storagerefusal"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	"github.com/oxidecomputer/oxide.go/oxide"
	corev1 "k8s.io/api/core/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	corecontrollers "sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	karpentercore "sigs.k8s.io/karpenter/pkg/operator"
)

func main() {
	if err := run(); err != nil {
		fmt.Printf("error while starting Oxide Karpenter: %v", err)
		os.Exit(1)
	}
}

const (
	envOxideHost    = "OXIDE_HOST"
	envOxideToken   = "OXIDE_TOKEN"
	envOxideProject = "OXIDE_PROJECT"

	envInstanceTypeIncrementPercent = "OXIDE_INSTANCE_TYPE_INCREMENT_PERCENT"

	envClusterID = "OXIDE_CLUSTER_ID"

	envLaunchDeadline                 = "OXIDE_LAUNCH_DEADLINE"
	envLaunchCleanupDeadline          = "OXIDE_LAUNCH_CLEANUP_DEADLINE"
	envLaunchDiskCreatingDeadline     = "OXIDE_LAUNCH_DISK_CREATING_DEADLINE"
	envLaunchDiskDeletingDeadline     = "OXIDE_LAUNCH_DISK_DELETING_DEADLINE"
	envLaunchInstanceCreatingDeadline = "OXIDE_LAUNCH_INSTANCE_CREATING_DEADLINE"
	envLaunchInstanceStartingDeadline = "OXIDE_LAUNCH_INSTANCE_STARTING_DEADLINE"
	envLaunchInstanceStoppingDeadline = "OXIDE_LAUNCH_INSTANCE_STOPPING_DEADLINE"

	envInventoryEnabled = "OXIDE_INVENTORY_ENABLED"
)

const (
	quotaRefreshPeriod                = time.Minute
	inventoryRefreshPeriod            = 5 * time.Minute
	capacityPublishPeriod             = 15 * time.Second
	inventoryRepairTolerationDuration = 30 * time.Minute
	apiRequestTimeout                 = 60 * time.Second
	reaperPeriod                      = time.Minute
	reaperQuarantine                  = 5 * time.Minute
)

func run() error {
	ctx, karpcore := karpentercore.NewOperator()

	host, token, project := os.Getenv(
		envOxideHost,
	), os.Getenv(
		envOxideToken,
	), os.Getenv(
		envOxideProject,
	)
	for name, value := range map[string]string{envOxideHost: host, envOxideToken: token, envOxideProject: project} {
		if value == "" {
			return fmt.Errorf("%s must be set", name)
		}
	}

	ox, err := oxide.NewClient(
		oxide.WithHost(host),
		oxide.WithToken(token),
		oxide.WithTimeout(apiRequestTimeout),
	)
	if err != nil {
		return err
	}

	bootstrappers := bootstrap.New(
		rke2.NewBootstrapper(karpcore.GetAPIReader()),
		kubeadm.NewBootstrapper(karpcore.GetAPIReader(), karpcore.GetClient()),
	)

	inventoryCfg := inventoryConfigFromEnv()
	var sledProvider *sled.Provider
	if inventoryCfg.enabled {
		sledProvider = sled.NewProvider(ox, project, inventoryRefreshPeriod)
	}

	quotas := utilization.NewProvider(ox, quotaRefreshPeriod/4)

	shapeCfg, err := shapeConfigFromEnv()
	if err != nil {
		return err
	}
	availabilityManager := availability.New(inventoryProvider(sledProvider))
	storageRefusals := storagerefusal.NewTracker()
	instanceTypes, err := instancetype.NewProviderWithShapeConfig(
		quotas,
		availabilityManager,
		shapeCfg,
	)
	if err != nil {
		return err
	}

	images := image.NewProvider(ox, providers.CoalesceWindow)
	subnets := subnet.NewProvider(ox, providers.CoalesceWindow)
	sshKeys := sshkey.NewProvider(ox, providers.CoalesceWindow)
	ipPools := ippool.NewProvider(ox, providers.CoalesceWindow)
	affinityGroups := affinitygroup.NewProvider(ox, project, providers.CoalesceWindow)

	snapshots := snapshot.NewProvider(ox, project, providers.CoalesceWindow)

	clusterUID, err := ownership.ClusterUID(ctx, karpcore.GetAPIReader(), os.Getenv(envClusterID))
	if err != nil {
		return err
	}
	deadlines, err := deadlinesFromEnv()
	if err != nil {
		return err
	}
	disks := diskprovider.NewProvider(
		ox,
		project,
		diskprovider.WithDeleteTimeout(deadlines.DiskDeleting),
	)
	bootDisks := bootdisk.NewProvider(
		karpcore.GetClient(),
		karpcore.GetAPIReader(),
		disks,
		clusterUID,
	)

	instanceProvider := instance.NewProvider(
		ox,
		project,
		func(ctx context.Context, released instance.Released) {
			availabilityManager.RecordCapacityReleased(ctx, availability.Shape{
				Ncpus: released.Ncpus,

				MemInGiB: int(released.Memory / cloudprovider.GiB),
			})
		},
		instance.WithDeadlines(deadlines),
		instance.WithDiskProvider(disks),
	)

	oxideCloudProivder := cloudprovider.NewOxideCloudProvider(
		karpcore.GetClient(),
		ox,
		project,
		clusterUID,
		instanceProvider,
		bootDisks,
		instanceTypes,
		karpcore.InstanceTypeStore,
		availabilityManager,
		storageRefusals,
		bootstrappers,
		inventoryCfg.repairPolicies(),

		[]providers.Drifter{
			images, storage.NewDrifter(), subnets, sshKeys, ipPools, affinityGroups,
		},
	)
	overlayUndecoratedCloudProvider := metrics.Decorate(oxideCloudProivder)
	cloudProvider := overlay.Decorate(
		overlayUndecoratedCloudProvider,
		karpcore.GetClient(),
		karpcore.InstanceTypeStore,
	)
	clusterState := state.NewCluster(karpcore.Clock, karpcore.GetClient(), cloudProvider)

	controllers := append(
		corecontrollers.NewControllers(
			ctx,
			karpcore.Manager,
			karpcore.Clock,
			karpcore.GetClient(),
			karpcore.EventRecorder,
			cloudProvider,
			overlayUndecoratedCloudProvider,
			clusterState,
			karpcore.InstanceTypeStore,
		),
		nodeclass.NewController(
			karpcore.GetClient(),
			karpcore.GetAPIReader(),
			karpcore.EventRecorder,
			images,
			subnets,
			sshKeys,
			ipPools,
			affinityGroups,
			snapshots,
			bootstrappers,
		),

		nodeclass.NewHashController(karpcore.GetClient()),

		bootdiskcontroller.NewController(
			karpcore.GetClient(),
			karpcore.GetAPIReader(),
			bootDisks,
			quotas,
		),

		capacitycontroller.NewController(
			karpcore.GetClient(),
			instanceTypes,
			availabilityManager,
			storageRefusals,
			capacityPublishPeriod,
		),

		affinitycontroller.NewController(karpcore.GetClient(), ox),

		launchcontroller.NewController(karpcore.GetClient(), ox, deadlines),

		reapercontroller.NewController(
			karpcore.GetClient(),
			karpcore.GetAPIReader(),
			ox,
			project,
			clusterUID,
			instanceProvider,
			reaperPeriod,
			reaperQuarantine,
		),

		utilizationcontroller.NewController(quotas, quotaRefreshPeriod),
	)
	if inventoryCfg.enabled {
		controllers = append(controllers,
			inventory.NewRefreshController(sledProvider, inventoryRefreshPeriod),
			inventory.NewController(karpcore.GetClient(), sledProvider, inventoryRefreshPeriod),
		)
	}

	karpcore.WithControllers(ctx, controllers...).Start(ctx)

	return nil
}

func shapeConfigFromEnv() (instancetype.ShapeConfig, error) {
	cfg := instancetype.DefaultShapeConfig()
	raw := os.Getenv(envInstanceTypeIncrementPercent)
	if raw == "" {
		return cfg, nil
	}

	increment, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return instancetype.ShapeConfig{}, fmt.Errorf(
			"%s is not a valid percentage: %w", envInstanceTypeIncrementPercent, err,
		)
	}
	cfg.IncrementPercent = increment
	if err := cfg.Validate(); err != nil {
		return instancetype.ShapeConfig{}, fmt.Errorf(
			"invalid %s: %w", envInstanceTypeIncrementPercent, err,
		)
	}
	return cfg, nil
}

func deadlinesFromEnv() (instance.Deadlines, error) {
	deadlines := instance.DefaultDeadlines()

	for _, field := range []struct {
		name   string
		target *time.Duration
	}{
		{envLaunchDeadline, &deadlines.Launch},
		{envLaunchCleanupDeadline, &deadlines.Cleanup},
		{envLaunchDiskCreatingDeadline, &deadlines.DiskCreating},
		{envLaunchDiskDeletingDeadline, &deadlines.DiskDeleting},
		{envLaunchInstanceCreatingDeadline, &deadlines.InstanceCreating},
		{envLaunchInstanceStartingDeadline, &deadlines.InstanceStarting},
		{envLaunchInstanceStoppingDeadline, &deadlines.InstanceStopping},
	} {
		raw := os.Getenv(field.name)
		if raw == "" {
			continue
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return instance.Deadlines{}, fmt.Errorf(
				"%s is not a valid duration: %w", field.name, err,
			)
		}
		*field.target = parsed
	}

	if err := deadlines.Validate(); err != nil {
		return instance.Deadlines{}, fmt.Errorf("invalid launch deadlines: %w", err)
	}
	return deadlines, nil
}

func inventoryProvider(sledProvider *sled.Provider) availability.InventoryProvider {
	if sledProvider == nil {
		return nil
	}
	return sledProvider
}

type inventoryConfig struct {
	enabled bool
}

func inventoryConfigFromEnv() inventoryConfig {
	enabled, _ := strconv.ParseBool(os.Getenv(envInventoryEnabled))
	return inventoryConfig{enabled: enabled}
}

func (c inventoryConfig) repairPolicies() []karpcloudprovider.RepairPolicy {
	if !c.enabled {
		return nil
	}
	return []karpcloudprovider.RepairPolicy{{
		ConditionType:      v1alpha1.ConditionTypeSledUnhealthy,
		ConditionStatus:    corev1.ConditionTrue,
		TolerationDuration: inventoryRepairTolerationDuration,
	}}
}
