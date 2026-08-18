package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cloudprovider"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/inventory"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/nodeclass"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/kubeadm"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/rke2"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/sled"
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

// Oxide API configuration. The token is read from the environment so it can be sourced from a
// Secret rather than being baked into the image.
const (
	envOxideHost    = "OXIDE_HOST"
	envOxideToken   = "OXIDE_TOKEN"
	envOxideProject = "OXIDE_PROJECT"

	// Sled inventory configuration. Inventory reads the fleet-scoped system API, which is a far
	// broader grant than provisioning into a single project, so it is off unless asked for and nothing
	// else depends on it.
	envInventoryEnabled                  = "OXIDE_INVENTORY_ENABLED"
	envInventoryRefreshPeriod            = "OXIDE_INVENTORY_REFRESH_PERIOD"
	envInventoryRepairTolerationDuration = "OXIDE_INVENTORY_REPAIR_TOLERATION_DURATION"
)

// defaultInventoryRefreshPeriod is how often the sled inventory is re-read. Building it costs a call
// per sled, and what it observes (which sled an instance is on, and that sled's state and policy)
// changes on the timescale of migrations and hardware maintenance, so minutes is the right order.
const defaultInventoryRefreshPeriod = 5 * time.Minute

// defaultInventoryRepairTolerationDuration is how long a node is left on a sled that is going out of
// service before Karpenter replaces it.
//
// This is deliberately not short. A sled is expunged or closed to new placements by an operator, not
// by a health check, so the condition is not acute and there is nothing to race; what a short duration
// would buy is churn if a policy is flipped and flipped back. It is long enough to be an obvious
// deliberate act and short enough to finish inside a maintenance window.
const defaultInventoryRepairTolerationDuration = 30 * time.Minute

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

	ox, err := oxide.NewClient(oxide.WithHost(host), oxide.WithToken(token))
	if err != nil {
		return err
	}

	// The registry is assembled here so that pkg/bootstrap does not have to import its own
	// implementations. Adding a bootstrap provider means adding it to this list.
	// Reads go through the uncached API reader so that fetching a single Secret or ConfigMap by name
	// does not make the manager list and watch every one of them in the cluster.
	bootstrappers := bootstrap.New(
		rke2.NewBootstrapper(karpcore.GetAPIReader()),
		kubeadm.NewBootstrapper(karpcore.GetAPIReader(), karpcore.GetClient()),
	)

	// Resolved before the cloud provider is built because the cloud provider has to advertise its
	// repair policies at construction time, and there is a policy to advertise only when inventory is
	// enabled.
	inventoryCfg, err := inventoryConfigFromEnv()
	if err != nil {
		return err
	}
	var sledProvider *sled.Provider
	if inventoryCfg.enabled {
		// The on-demand refresh floor is deliberately tighter than the scheduled period so that a node
		// launched between refreshes gets labeled promptly, while a cluster full of unplaceable nodes
		// still cannot drive more than one rack walk per floor.
		sledProvider = sled.NewProvider(ox, project, inventoryCfg.refreshPeriod/4)
	}

	oxideCloudProivder := cloudprovider.NewOxideCloudProvider(
		karpcore.GetClient(),
		ox,
		project,
		instancetype.NewProvider(),
		bootstrappers,
		inventoryCfg.repairPolicies(),
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
		nodeclass.NewController(karpcore.GetClient(), karpcore.GetAPIReader(), ox, bootstrappers),
	)

	if inventoryCfg.enabled {
		controllers = append(controllers,
			inventory.NewRefreshController(sledProvider, inventoryCfg.refreshPeriod),
			inventory.NewController(karpcore.GetClient(), sledProvider, inventoryCfg.refreshPeriod),
		)
	}

	karpcore.WithControllers(ctx, controllers...).Start(ctx)

	return nil
}

// inventoryConfig is the sled inventory configuration, resolved once at startup.
type inventoryConfig struct {
	enabled       bool
	refreshPeriod time.Duration
	// repairTolerationDuration is how long a node is allowed to sit on a sled that is going out of
	// service before Karpenter replaces it. Zero disables replacement while leaving the rest of
	// inventory on.
	repairTolerationDuration time.Duration
}

// inventoryConfigFromEnv reads the inventory configuration.
//
// Inventory is opt-in because reading it needs fleet-level access to the Oxide system API, which an
// operator may not want to hand to Karpenter. Nothing in the provider reads what it writes, so leaving
// it off is a supported configuration rather than a degraded one.
//
// A malformed duration is the one thing here worth refusing to start over. Everything else inventory
// can get wrong at runtime degrades to "no placement labels", but a bad duration is a static mistake
// in the operator's own configuration, and silently substituting a default would hide it.
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
			// Zero is meaningful here: it turns replacement off and leaves the labels, conditions and
			// annotations in place, which is the configuration for wanting the observability without the
			// disruption.
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

// repairPolicies is what the cloud provider advertises to Karpenter's node repair.
//
// It is empty unless inventory is enabled, since nothing else sets the condition it names, and empty
// is also what stops core from registering its health controller for this provider at all. Node repair
// additionally requires Karpenter's own NodeRepair feature gate, so acting on the condition takes both
// this and that gate; the condition itself is recorded either way as sled health.
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
