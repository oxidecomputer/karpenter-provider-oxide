package controller

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	karpapis "sigs.k8s.io/karpenter/pkg/apis"
)

// requiredResources gate manager start until these CRDs are served. Core
// registers its indexes once at startup and skips any whose CRD is missing, so
// NodePools never see their OxideNodeClass go Ready until a restart.
// This case can occur when the controller starts before the CRD chart is installed.
var requiredResources = map[string][]string{
	karpapis.Group + "/v1":         {"nodepools", "nodeclaims"},
	v1alpha1.GroupVersion.String(): {"oxidenodeclasses"},
}

const crdPollInterval = 2 * time.Second

// waitForCRDs blocks until the API serves every required resource.
func waitForCRDs(
	ctx context.Context,
	client discovery.DiscoveryInterface,
	interval time.Duration,
) error {
	logged := false
	for {
		missing := missingResources(client)
		if len(missing) == 0 {
			return nil
		}
		if !logged {
			fmt.Fprintf(
				os.Stderr,
				"waiting for CRDs to be served: %s\n",
				strings.Join(missing, ", "),
			)
			logged = true
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for CRDs %s: %w", strings.Join(missing, ", "), ctx.Err())
		case <-time.After(interval):
		}
	}
}

func missingResources(client discovery.DiscoveryInterface) []string {
	var missing []string
	for groupVersion, resources := range requiredResources {
		list, err := client.ServerResourcesForGroupVersion(groupVersion)
		for _, resource := range resources {
			if err != nil || list == nil ||
				!slices.ContainsFunc(
					list.APIResources,
					func(r metav1.APIResource) bool { return r.Name == resource },
				) {
				missing = append(missing, resource+"."+groupVersion)
			}
		}
	}
	slices.Sort(missing)
	return missing
}
