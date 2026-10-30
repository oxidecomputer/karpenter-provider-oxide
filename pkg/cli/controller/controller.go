package controller

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
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cli/config"
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
	projectprovider "github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/project"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/snapshot"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/sshkey"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/storage"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/subnet"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/sled"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/storagerefusal"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/utilization"
	"github.com/oxidecomputer/oxide.go/oxide"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	corecontrollers "sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	karpentercore "sigs.k8s.io/karpenter/pkg/operator"
)

const (
	envOxideHost    = "OXIDE_HOST"
	envOxideToken   = "OXIDE_TOKEN"
	envOxideProject = "OXIDE_PROJECT"

	envClusterID = "OXIDE_CLUSTER_ID"

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

// NewCommand runs the controller-manager. Karpenter parses os.Args itself, so unknown
// flags are left for it.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "controller",
		Short:              "Run the Karpenter controller-manager",
		Args:               cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE: func(*cobra.Command, []string) error {
			return Run()
		},
	}
}

// Run starts the controller-manager and blocks until it stops.
func Run() error {
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

	shapeCfg, err := config.ShapeConfigFromEnv()
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
	me, err := ox.CurrentUserView(ctx)
	if err != nil {
		return fmt.Errorf("unable to read the token's silo: %w", err)
	}
	instanceTypes.SetTopology(instancetype.SiloTopology(string(me.SiloName)))

	projects := projectprovider.NewResolver(ox, project, providers.CoalesceWindow)
	images := image.NewProvider(ox, projects, providers.CoalesceWindow)
	subnets := subnet.NewProvider(ox, projects, providers.CoalesceWindow)
	sshKeys := sshkey.NewProvider(ox, providers.CoalesceWindow)
	ipPools := ippool.NewProvider(ox, providers.CoalesceWindow)
	affinityGroups := affinitygroup.NewProvider(ox, project, providers.CoalesceWindow)

	snapshots := snapshot.NewProvider(ox, projects, providers.CoalesceWindow)

	clusterUID, err := ownership.ClusterUID(ctx, karpcore.GetAPIReader(), os.Getenv(envClusterID))
	if err != nil {
		return err
	}
	disks := diskprovider.NewProvider(ox, project)
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
			}, availability.Platform(released.CPUPlatform))
		},
		instance.WithDeadlines(instance.DefaultDeadlines()),
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
		repairPolicies(),

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
	// Nil unless the NodeRepair gate is on; the disruption repair method
	// panics if cluster state was built without it.
	repairPolicyMatcher, err := health.NewRepairPolicyMatcher(ctx, cloudProvider)
	if err != nil {
		return err
	}
	clusterState := state.NewCluster(
		karpcore.Clock,
		karpcore.GetClient(),
		cloudProvider,
		state.WithRepairPolicyMatcher(repairPolicyMatcher),
	)

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
			karpcore.PredictionStore,
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

		launchcontroller.NewController(karpcore.GetClient(), ox),

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

// Defined even without inventory, which is what sets the condition: core
// refuses to start NodeRepair against a provider with no policies.
func repairPolicies() []karpcloudprovider.RepairPolicy {
	return []karpcloudprovider.RepairPolicy{{
		ConditionType:      v1alpha1.ConditionTypeSledUnhealthy,
		ConditionStatus:    corev1.ConditionTrue,
		TolerationDuration: inventoryRepairTolerationDuration,
	}}
}
