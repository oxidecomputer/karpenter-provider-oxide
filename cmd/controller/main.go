package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/affinitypolicy"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/cloudprovider"
	affinitycontroller "github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/affinity"
	capacitycontroller "github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/capacity"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/inventory"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/nodeclass"
	utilizationcontroller "github.com/oxidecomputer/karpenter-provider-oxide/pkg/controllers/utilization"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/kubeadm"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/image"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/subnet"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/rke2"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/sled"
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

	// Silo quota configuration. Unlike inventory this reads a silo-scoped endpoint that needs no
	// permissions beyond launching instances, so it defaults to on.
	envQuotaEnabled       = "OXIDE_QUOTA_ENABLED"
	envQuotaRefreshPeriod = "OXIDE_QUOTA_REFRESH_PERIOD"

	// Resource provider cache configuration. One knob per cadence rather than one per provider,
	// because what differs between the two is how fast the resource itself moves, and an operator who
	// wants images picked up sooner wants that for images rather than for one image provider.
	envImageTTL    = "OXIDE_IMAGE_TTL"
	envResourceTTL = "OXIDE_RESOURCE_TTL"
)

// defaultImageTTL is how long a resolution of a nodeclass's image selectors is served before the
// images are listed again.
//
// The shortest of the two, and equal to the nodeclass reconcile interval, so images are re-resolved
// on every reconcile. That is the point of a wildcard selector: a newly uploaded image it matches
// should be launched without anybody editing the nodeclass, and this is what bounds how long that
// takes.
const defaultImageTTL = 5 * time.Minute

// defaultResourceTTL is how long a resolution of the subnet, SSH key, IP pool and affinity group
// selectors is served.
//
// Six times the reconcile interval, so five reconciles out of six cost no Oxide API traffic at all.
// These resources are created by an operator rather than by a pipeline, so a selector's meaning
// changes on the timescale of someone changing the project deliberately, and a stale reading of one
// costs nothing until the next launch reads it from status.
const defaultResourceTTL = 30 * time.Minute

// defaultQuotaRefreshPeriod is how often the silo's quota is re-read.
//
// This is one API call, and what it measures moves whenever anything in the silo starts or stops, so
// it is polled far more often than the sled inventory. It is not the only line of defense: a launch
// that exceeds the quota is still refused by the API and diagnosed from the snapshot, so a stale
// reading costs a wasted launch rather than a wrong decision.
const defaultQuotaRefreshPeriod = time.Minute

// defaultInventoryRefreshPeriod is how often the sled inventory is re-read. Building it costs a call
// per sled, and what it observes (which sled an instance is on, and that sled's state and policy)
// changes on the timescale of migrations and hardware maintenance, so minutes is the right order.
const defaultInventoryRefreshPeriod = 5 * time.Minute

// capacityPublishPeriod is how often the capacity view is rendered.
//
// It is not a refresh: everything it reports is already in memory, so this only decides how quickly a
// change shows up in the object and the metrics. Writes are skipped when nothing has changed, so a
// short period costs a render rather than API traffic, and being able to watch a backoff count down is
// worth more than the render costs.
const capacityPublishPeriod = 15 * time.Second

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
		sledProvider = sled.NewProvider(ox, project, inventoryCfg.refreshPeriod)
	}

	// Quota awareness needs no permissions beyond launching instances, since a silo can read its own
	// utilization, so it is on unless explicitly switched off. A quota that cannot be read constrains
	// nothing rather than blocking launches, so the failure mode of leaving it on is the same as
	// turning it off.
	quotaCfg, err := quotaConfigFromEnv()
	if err != nil {
		return err
	}
	var quotas *utilization.Provider
	if quotaCfg.enabled {
		// The on-demand floor is tighter than the poll interval so that a burst of refused launches
		// re-reads the quota promptly, without every one of them issuing its own read.
		quotas = utilization.NewProvider(ox, quotaCfg.refreshPeriod/4)
	}

	// Built here rather than inside the cloud provider because two things need it: the cloud provider
	// decides launches with it, and the capacity controller publishes what it currently believes.
	//
	// Both dependencies are passed as interfaces, so a nil provider has to be dropped rather than handed
	// over: a typed nil inside a non-nil interface would satisfy the nil check and then panic.
	instanceTypes := instancetype.NewProvider(
		quotaProvider(quotas),
		inventoryProvider(sledProvider),
	)

	// Shared between the launch path, which records refusals into it, and the nodeclass controller,
	// which publishes them as the condition that holds a backing-off nodeclass out of provisioning.
	affinityPolicy := affinitypolicy.NewTracker()

	// The resource providers are built once and handed to both consumers, which is what makes their
	// caches shared: two nodeclasses selecting the same images ask one question and get one answer.
	// The nodeclass controller resolves through them and publishes the result to status; the cloud
	// provider only asks them about drift, against the status the controller published.
	providerTTLs, err := providerTTLsFromEnv()
	if err != nil {
		return err
	}
	images := image.NewProvider(ox, providerTTLs.image)
	subnets := subnet.NewProvider(ox, providerTTLs.resource)

	oxideCloudProivder := cloudprovider.NewOxideCloudProvider(
		karpcore.GetClient(),
		ox,
		project,
		instanceTypes,
		affinityPolicy,
		bootstrappers,
		inventoryCfg.repairPolicies(),
		// In resolution order. Which reason wins when several apply is this order, and it is asserted
		// by pkg/cloudprovider's tests.
		[]providers.Drifter{images, subnets},
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
			ox,
			project,
			images,
			subnets,
			affinityPolicy,
			bootstrappers,
		),
		// Separate from the reconciler above so that the hash a nodeclass publishes cannot be held
		// back by a failure to reach the Oxide API. A launch records the hash it computed, so a stamp
		// lagging the spec reads as every node launched in the meantime having drifted.
		nodeclass.NewHashController(karpcore.GetClient()),
		// Registered unconditionally. The refusal history it reports is there whether or not inventory
		// and quota awareness are, and a view that disappeared when they were switched off would be
		// missing exactly when an operator is trying to work out why nothing is scaling.
		capacitycontroller.NewController(
			karpcore.GetClient(),
			instanceTypes,
			capacityPublishPeriod,
		),
		// Registered unconditionally, and inert on a NodeClaim with no recorded group memberships,
		// which is every one of them until a nodeclass opts into spec.affinity.
		affinitycontroller.NewController(karpcore.GetClient(), ox),
	)

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

// quotaConfig is the silo quota configuration, resolved once at startup.
type quotaConfig struct {
	enabled       bool
	refreshPeriod time.Duration
}

// quotaConfigFromEnv reads the quota configuration. Unlike inventory this defaults to on: reading the
// silo's own utilization needs no permissions beyond what launching an instance already requires.
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

// quotaProvider adapts a possibly-nil provider to the interface the instance type provider takes.
//
// A *utilization.Provider that is nil cannot simply be passed: assigning it to a non-nil interface
// value would make every nil check downstream pass and then panic on the first call. Returning an
// untyped nil is the only thing that reads as "no quota provider".
func quotaProvider(quotas *utilization.Provider) instancetype.QuotaProvider {
	if quotas == nil {
		return nil
	}
	return quotas
}

// inventoryProvider adapts a possibly-nil sled provider to the interface the instance type provider
// takes, for the same reason quotaProvider does: an untyped nil is the only value that reads as "there
// is no inventory".
//
// This is the one place the two halves of inventory meet. The controllers read the rack for
// observability, and handing the same snapshot to the instance type provider turns it into a
// scheduling input: shapes no sled has room for stop being offered at all. With inventory off, that
// input is simply absent and the provider decides from what the rack refuses, as it always has.
func inventoryProvider(sledProvider *sled.Provider) instancetype.InventoryProvider {
	if sledProvider == nil {
		return nil
	}
	return sledProvider
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

// providerTTLs is how long each resource provider serves a resolution before making it again.
type providerTTLs struct {
	image    time.Duration
	resource time.Duration
}

// providerTTLsFromEnv reads the cache freshness configuration.
//
// A malformed or non-positive duration is fatal, for the same reason the inventory refresh period is:
// it is a static mistake in the operator's own configuration, and quietly substituting a default
// would hide a nodeclass being refreshed on a cadence nobody asked for.
func providerTTLsFromEnv() (providerTTLs, error) {
	ttls := providerTTLs{image: defaultImageTTL, resource: defaultResourceTTL}
	for _, field := range []struct {
		name   string
		target *time.Duration
	}{
		{envImageTTL, &ttls.image},
		{envResourceTTL, &ttls.resource},
	} {
		raw := os.Getenv(field.name)
		if raw == "" {
			continue
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return providerTTLs{}, fmt.Errorf("%s is not a valid duration: %w", field.name, err)
		}
		if parsed <= 0 {
			return providerTTLs{}, fmt.Errorf("%s must be positive, got %s", field.name, parsed)
		}
		*field.target = parsed
	}
	return ttls, nil
}
