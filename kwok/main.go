// The KWOK bridge is separate from cmd/controller: the production provider owns all cloud mutations.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/kwok/audit"
	"github.com/oxidecomputer/karpenter-provider-oxide/kwok/controllers/node"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/oxide.go/oxide"

	"k8s.io/client-go/discovery"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	_ "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "audit" {
		return audit.Run(os.Args[2:])
	}
	poll := flag.Duration("poll-interval", 5*time.Second, "Nexus observation interval")
	stale := flag.Duration(
		"stale-after",
		time.Minute,
		"Grace period before failed observations become Unknown",
	)
	timeout := flag.Duration("api-timeout", 60*time.Second, "Nexus request timeout")
	concurrency := flag.Int("concurrency", 2, "Maximum simultaneous claim reconciliations")
	taints := flag.String(
		"remove-startup-taints",
		"",
		"Comma-separated startup taint key:effect allowlist",
	)
	leader := flag.Bool("leader-elect", true, "Use a dedicated bridge leader-election Lease")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	if *poll <= 0 || *stale <= 0 || *timeout <= 0 || *concurrency < 1 {
		return fmt.Errorf("durations and concurrency must be positive")
	}
	for _, key := range []string{"OXIDE_HOST", "OXIDE_TOKEN", "OXIDE_PROJECT"} {
		if os.Getenv(key) == "" {
			return fmt.Errorf("%s must be set", key)
		}
	}
	var allow []string
	if *taints != "" {
		for item := range strings.SplitSeq(*taints, ",") {
			key, effect, ok := strings.Cut(item, ":")
			if !ok || key == "" ||
				(effect != "NoSchedule" && effect != "NoExecute" && effect != "PreferNoSchedule") {
				return fmt.Errorf("invalid startup taint %q; use key:effect", item)
			}
			allow = append(allow, item)
		}
	}
	ox, err := oxide.NewClient(
		oxide.WithHost(os.Getenv("OXIDE_HOST")),
		oxide.WithToken(os.Getenv("OXIDE_TOKEN")),
		oxide.WithTimeout(*timeout),
	)
	if err != nil {
		return err
	}
	scheme := clientscheme.Scheme
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(
		cfg,
		ctrl.Options{
			Scheme:                  scheme,
			LeaderElection:          *leader,
			LeaderElectionID:        "oxide-kwok-bridge",
			LeaderElectionNamespace: os.Getenv("SYSTEM_NAMESPACE"),
			HealthProbeBindAddress:  ":8081",
			Metrics:                 metricsserver.Options{BindAddress: ":8080"},
		},
	)
	if err != nil {
		return err
	}
	ctx := ctrl.SetupSignalHandler()
	cluster, err := ownership.ClusterUID(ctx, mgr.GetAPIReader(), os.Getenv("OXIDE_CLUSTER_ID"))
	if err != nil {
		return err
	}
	projectCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	project, err := ox.ProjectView(
		projectCtx,
		oxide.ProjectViewParams{Project: oxide.NameOrId(os.Getenv("OXIDE_PROJECT"))},
	)
	if err != nil {
		return fmt.Errorf("resolve configured project: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	version, err := discoveryClient.ServerVersion()
	if err != nil {
		return err
	}
	bridge := &node.Controller{
		Client:              mgr.GetClient(),
		Reader:              mgr.GetAPIReader(),
		Instances:           ox,
		ClusterUID:          cluster,
		ProjectID:           project.Id,
		KubernetesVersion:   version.GitVersion,
		PollInterval:        *poll,
		StaleAfter:          *stale,
		Clock:               clock.RealClock{},
		RemoveStartupTaints: allow,
	}
	if err := bridge.Setup(mgr, *concurrency); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctx)
}
