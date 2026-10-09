// audit only reads cloud inventory. Failed listings are never reported as empty.
package audit

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/ownership"
	"github.com/oxidecomputer/oxide.go/oxide"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type entry struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	ClaimUID string `json:"claimUID"`
	Claim    string `json:"claim"`
}

func Run(args []string) error {
	flags := flag.NewFlagSet("audit", flag.ContinueOnError)
	empty := flags.Bool("require-empty", false, "Fail if matching cloud resources remain")
	uids := flags.String(
		"claim-uids",
		"",
		"Comma-separated claim UIDs; empty audits the entire cluster",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	for _, key := range []string{"OXIDE_HOST", "OXIDE_TOKEN", "OXIDE_PROJECT"} {
		if os.Getenv(key) == "" {
			return fmt.Errorf("%s must be set", key)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	k, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return err
	}
	cluster, err := ownership.ClusterUID(ctx, k, os.Getenv("OXIDE_CLUSTER_ID"))
	if err != nil {
		return err
	}
	ox, err := oxide.NewClient(
		oxide.WithHost(os.Getenv("OXIDE_HOST")),
		oxide.WithToken(os.Getenv("OXIDE_TOKEN")),
		oxide.WithTimeout(30*time.Second),
	)
	if err != nil {
		return err
	}
	entries := []entry{}
	add := func(id, description string) {
		m, ok := ownership.Parse(description)
		if !ok || m.ClusterUID != cluster {
			return
		}
		if *uids != "" && !slices.Contains(strings.Split(*uids, ","), string(m.NodeClaimUID)) {
			return
		}
		entries = append(
			entries,
			entry{string(m.Kind), id, string(m.NodeClaimUID), m.NodeClaimName},
		)
	}
	for page := ""; ; {
		result, err := ox.InstanceList(
			ctx,
			oxide.InstanceListParams{
				Project:   oxide.NameOrId(os.Getenv("OXIDE_PROJECT")),
				PageToken: page,
			},
		)
		if err != nil {
			return fmt.Errorf("instance audit incomplete: %w", err)
		}
		for _, i := range result.Items {
			add(i.Id, i.Description)
		}
		if result.NextPage == "" {
			break
		}
		if result.NextPage == page {
			return fmt.Errorf("repeated instance page token")
		}
		page = result.NextPage
	}
	for page := ""; ; {
		result, err := ox.DiskList(
			ctx,
			oxide.DiskListParams{
				Project:   oxide.NameOrId(os.Getenv("OXIDE_PROJECT")),
				PageToken: page,
			},
		)
		if err != nil {
			return fmt.Errorf("disk audit incomplete: %w", err)
		}
		for _, d := range result.Items {
			add(d.Id, d.Description)
		}
		if result.NextPage == "" {
			break
		}
		if result.NextPage == page {
			return fmt.Errorf("repeated disk page token")
		}
		page = result.NextPage
	}
	if err := json.NewEncoder(os.Stdout).Encode(entries); err != nil {
		return err
	}
	if *empty && len(entries) > 0 {
		return fmt.Errorf("%d owned cloud resources remain", len(entries))
	}
	return nil
}
