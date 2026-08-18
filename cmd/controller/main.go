package main

import (
	"fmt"
	"os"

	"github.com/oxidecomputer/karpenter/pkg/bootstrap"
	"github.com/oxidecomputer/karpenter/pkg/cloudprovider"
	"github.com/oxidecomputer/karpenter/pkg/controllers/nodeclass"
	"github.com/oxidecomputer/karpenter/pkg/instancetype"
	"github.com/oxidecomputer/karpenter/pkg/kubeadm"
	"github.com/oxidecomputer/karpenter/pkg/rke2"
	"github.com/oxidecomputer/oxide.go/oxide"
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

	oxideCloudProivder := cloudprovider.NewOxideCloudProvider(
		karpcore.GetClient(),
		ox,
		project,
		instancetype.NewProvider(),
		bootstrappers,
	)
	overlayUndecoratedCloudProvider := metrics.Decorate(oxideCloudProivder)
	cloudProvider := overlay.Decorate(
		overlayUndecoratedCloudProvider,
		karpcore.GetClient(),
		karpcore.InstanceTypeStore,
	)
	clusterState := state.NewCluster(karpcore.Clock, karpcore.GetClient(), cloudProvider)

	karpcore.WithControllers(ctx, corecontrollers.NewControllers(
		ctx,
		karpcore.Manager,
		karpcore.Clock,
		karpcore.GetClient(),
		karpcore.EventRecorder,
		cloudProvider,
		overlayUndecoratedCloudProvider,
		clusterState,
		karpcore.InstanceTypeStore,
	)...).WithControllers(ctx, nodeclass.NewController(karpcore.GetClient(), karpcore.GetAPIReader(), ox, bootstrappers)).
		Start(ctx)

	return nil
}
