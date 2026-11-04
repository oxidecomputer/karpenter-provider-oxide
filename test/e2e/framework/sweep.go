package framework

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/apis/v1alpha1"
	"github.com/oxidecomputer/oxide.go/oxide"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Sweep deletes what earlier runs left behind: labeled pods, nodepools and nodeclasses,
// then ssh keys with the suite's prefix. Instances follow their nodepool through
// Karpenter. It takes out every run's objects, so it must not overlap a live suite.
func (e *Env) Sweep(ctx context.Context) error {
	selector := client.HasLabels{LabelRun}
	var pods corev1.PodList
	if err := e.Kube.List(ctx, &pods, selector); err != nil {
		return err
	}
	for i := range pods.Items {
		if err := e.deleteNow(ctx, &pods.Items[i]); err != nil {
			return err
		}
	}
	var pools karpv1.NodePoolList
	if err := e.Kube.List(ctx, &pools, selector); err != nil {
		return err
	}
	for i := range pools.Items {
		if err := e.deleteNow(ctx, &pools.Items[i]); err != nil {
			return err
		}
	}
	var classes v1alpha1.OxideNodeClassList
	if err := e.Kube.List(ctx, &classes, selector); err != nil {
		return err
	}
	for i := range classes.Items {
		if err := e.deleteNow(ctx, &classes.Items[i]); err != nil {
			return err
		}
	}
	if n := len(pods.Items) + len(pools.Items) + len(classes.Items); n > 0 {
		fmt.Fprintf(os.Stderr, "e2e: sweeping %d leftover objects from earlier runs\n", n)
		if err := e.waitGone(ctx, &classes); err != nil {
			return err
		}
	}

	keys, err := e.Oxide.CurrentUserSshKeyListAllPages(ctx, oxide.CurrentUserSshKeyListParams{})
	if err != nil {
		return fmt.Errorf("listing ssh keys: %w", err)
	}
	for _, key := range keys {
		if !strings.HasPrefix(string(key.Name), sshKeyPrefix) {
			continue
		}
		fmt.Fprintf(os.Stderr, "e2e: deleting stale ssh key %s\n", key.Name)
		if err := e.Oxide.CurrentUserSshKeyDelete(ctx, oxide.CurrentUserSshKeyDeleteParams{
			SshKey: oxide.NameOrId(key.Id),
		}); err != nil && !errors.Is(err, oxide.ErrObjectNotFound) {
			return fmt.Errorf("deleting ssh key %s: %w", key.Name, err)
		}
	}
	return nil
}

func (e *Env) deleteNow(ctx context.Context, obj client.Object) error {
	fmt.Fprintf(os.Stderr, "e2e: deleting leftover %T %s\n", obj, client.ObjectKeyFromObject(obj))
	err := e.Kube.Delete(ctx, obj, client.GracePeriodSeconds(0))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// waitGone blocks until the nodeclasses, whose finalizers hold until their nodeclaims
// are terminated, are deleted.
func (e *Env) waitGone(ctx context.Context, classes *v1alpha1.OxideNodeClassList) error {
	deadline := time.Now().Add(e.DeleteTimeout)
	for _, nc := range classes.Items {
		for {
			err := e.Kube.Get(ctx, client.ObjectKeyFromObject(&nc), &v1alpha1.OxideNodeClass{})
			if apierrors.IsNotFound(err) {
				break
			}
			if err != nil {
				return err
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("leftover OxideNodeClass %s is still deleting", nc.Name)
			}
			time.Sleep(e.Poll)
		}
	}
	return nil
}
