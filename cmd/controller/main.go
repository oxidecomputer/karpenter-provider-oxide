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

	envReaperEnabled    = "OXIDE_REAPER_ENABLED"
	envReaperPeriod     = "OXIDE_REAPER_PERIOD"
	envReaperQuarantine = "OXIDE_REAPER_QUARANTINE"

	envInventoryEnabled                  = "OXIDE_INVENTORY_ENABLED"
	envInventoryRefreshPeriod            = "OXIDE_INVENTORY_REFRESH_PERIOD"
	envInventoryRepairTolerationDuration = "OXIDE_INVENTORY_REPAIR_TOLERATION_DURATION"

	envQuotaEnabled       = "OXIDE_QUOTA_ENABLED"
	envQuotaRefreshPeriod = "OXIDE_QUOTA_REFRESH_PERIOD"
)

const defaultQuotaRefreshPeriod = time.Minute

const defaultInventoryRefreshPeriod = 5 * time.Minute

const capacityPublishPeriod = 15 * time.Second

const defaultInventoryRepairTolerationDuration = 30 * time.Minute

const apiRequestTimeout = 60 * time.Second

const defaultReaperPeriod = time.Minute

const defaultReaperQuarantine = 5 * time.Minute

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

	inventoryCfg, err := inventoryConfigFromEnv()
	if err != nil {
		return err
	}
	var sledProvider *sled.Provider
	if inventoryCfg.enabled {
		sledProvider = sled.NewProvider(ox, project, inventoryCfg.refreshPeriod)
	}

	quotaCfg, err := quotaConfigFromEnv()
	if err != nil {
		return err
	}
	var quotas *utilization.Provider
	if quotaCfg.enabled {

		quotas = utilization.NewProvider(ox, quotaCfg.refreshPeriod/4)
	}

	shapeCfg, err := shapeConfigFromEnv()
	if err != nil {
		return err
	}
	availabilityManager := availability.New(inventoryProvider(sledProvider))
	storageRefusals := storagerefusal.NewTracker()
	instanceTypes, err := instancetype.NewProviderWithShapeConfig(
		quotaProvider(quotas),
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
			images,
			subnets,
			sshKeys,
			ipPools,
			affinityGroups,
			snapshots,
			bootstrappers,
		),

		nodeclass.NewHashController(karpcore.GetClient()),

		bootdiskcontroller.NewController(karpcore.GetClient(), bootDisks, quotas),

		capacitycontroller.NewController(
			karpcore.GetClient(),
			instanceTypes,
			availabilityManager,
			storageRefusals,
			capacityPublishPeriod,
		),

		affinitycontroller.NewController(karpcore.GetClient(), ox),

		launchcontroller.NewController(karpcore.GetClient(), ox, deadlines),
	)

	reaperCfg, err := reaperConfigFromEnv()
	if err != nil {
		return err
	}
	if reaperCfg.enabled {
		controllers = append(controllers, reapercontroller.NewController(
			karpcore.GetClient(),

			karpcore.GetAPIReader(),
			ox,
			project,
			clusterUID,
			instanceProvider,
			reaperCfg.period,
			reaperCfg.quarantine,
		))
	}

	if inventoryCfg.enabled {
		controllers = append(controllers,
			inventory.NewRefreshController(sledProvider, inventoryCfg.refreshPeriod),
			inventory.NewController(karpcore.GetClient(), sledProvider, inventoryCfg.refreshPeriod),
		)
	}
	if quotaCfg.enabled {
		controllers = append(
			controllers,
			utilizationcontroller.NewController(quotas, quotaCfg.refreshPeriod),
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

type reaperConfig struct {
	enabled    bool
	period     time.Duration
	quarantine time.Duration
}

func reaperConfigFromEnv() (reaperConfig, error) {
	cfg := reaperConfig{
		enabled:    true,
		period:     defaultReaperPeriod,
		quarantine: defaultReaperQuarantine,
	}

	if raw := os.Getenv(envReaperEnabled); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return reaperConfig{}, fmt.Errorf(
				"%s is not a valid boolean: %w",
				envReaperEnabled,
				err,
			)
		}
		cfg.enabled = enabled
	}
	for _, field := range []struct {
		name   string
		target *time.Duration
	}{
		{envReaperPeriod, &cfg.period},
		{envReaperQuarantine, &cfg.quarantine},
	} {
		raw := os.Getenv(field.name)
		if raw == "" {
			continue
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return reaperConfig{}, fmt.Errorf("%s is not a valid duration: %w", field.name, err)
		}
		if parsed <= 0 {
			return reaperConfig{}, fmt.Errorf("%s must be positive, got %s", field.name, parsed)
		}
		*field.target = parsed
	}
	return cfg, nil
}

type quotaConfig struct {
	enabled       bool
	refreshPeriod time.Duration
}

func quotaConfigFromEnv() (quotaConfig, error) {
	cfg := quotaConfig{enabled: true, refreshPeriod: defaultQuotaRefreshPeriod}

	if raw := os.Getenv(envQuotaEnabled); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return quotaConfig{}, fmt.Errorf("%s is not a valid boolean: %w", envQuotaEnabled, err)
		}
		cfg.enabled = enabled
	}
	if raw := os.Getenv(envQuotaRefreshPeriod); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return quotaConfig{}, fmt.Errorf(
				"%s is not a valid duration: %w", envQuotaRefreshPeriod, err,
			)
		}
		if parsed <= 0 {
			return quotaConfig{}, fmt.Errorf(
				"%s must be positive, got %s", envQuotaRefreshPeriod, parsed,
			)
		}
		cfg.refreshPeriod = parsed
	}
	return cfg, nil
}

func quotaProvider(quotas *utilization.Provider) instancetype.QuotaProvider {
	if quotas == nil {
		return nil
	}
	return quotas
}

func inventoryProvider(sledProvider *sled.Provider) availability.InventoryProvider {
	if sledProvider == nil {
		return nil
	}
	return sledProvider
}

type inventoryConfig struct {
	enabled       bool
	refreshPeriod time.Duration

	repairTolerationDuration time.Duration
}

func inventoryConfigFromEnv() (inventoryConfig, error) {
	cfg := inventoryConfig{
		refreshPeriod:            defaultInventoryRefreshPeriod,
		repairTolerationDuration: defaultInventoryRepairTolerationDuration,
	}
	if enabled, _ := strconv.ParseBool(os.Getenv(envInventoryEnabled)); !enabled {
		return cfg, nil
	}
	cfg.enabled = true

	for _, field := range []struct {
		name    string
		target  *time.Duration
		allowed func(time.Duration) bool
		must    string
	}{
		{
			name:    envInventoryRefreshPeriod,
			target:  &cfg.refreshPeriod,
			allowed: func(d time.Duration) bool { return d > 0 },
			must:    "be positive",
		},
		{

			name:    envInventoryRepairTolerationDuration,
			target:  &cfg.repairTolerationDuration,
			allowed: func(d time.Duration) bool { return d >= 0 },
			must:    "not be negative",
		},
	} {
		raw := os.Getenv(field.name)
		if raw == "" {
			continue
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return inventoryConfig{}, fmt.Errorf("%s is not a valid duration: %w", field.name, err)
		}
		if !field.allowed(parsed) {
			return inventoryConfig{}, fmt.Errorf(
				"%s must %s, got %s", field.name, field.must, parsed,
			)
		}
		*field.target = parsed
	}

	return cfg, nil
}

func (c inventoryConfig) repairPolicies() []karpcloudprovider.RepairPolicy {
	if !c.enabled || c.repairTolerationDuration == 0 {
		return nil
	}
	return []karpcloudprovider.RepairPolicy{{
		ConditionType:      v1alpha1.ConditionTypeSledUnhealthy,
		ConditionStatus:    corev1.ConditionTrue,
		TolerationDuration: c.repairTolerationDuration,
	}}
}
