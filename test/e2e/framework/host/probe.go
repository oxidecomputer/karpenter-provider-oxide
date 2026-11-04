// Package host exposes typed facts about one node to the e2e suite. Facts come
// from the Kubernetes and Oxide APIs where they can; anything that only exists
// inside the guest goes through a privileged probe pod, created lazily and
// deleted with the test.
package host

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/ptr"
)

// Probe is one privileged pod pinned to a node, through which commands run
// inside the host's own namespaces.
type Probe struct {
	clientset *kubernetes.Clientset
	config    *rest.Config
	namespace string
	image     string
	node      string
	name      string
	labels    map[string]string
}

func probeImage() string {
	if image := os.Getenv("E2E_PROBE_IMAGE"); image != "" {
		return image
	}
	return "docker.io/library/busybox:1.37.0"
}

// Create runs a probe pod on the node and waits for it to be running. The pod
// is privileged, shares the host's PID namespace, and tolerates every taint, so
// nsenter can reach PID 1's mount, network and IPC namespaces.
func Create(
	ctx context.Context,
	config *rest.Config,
	namespace string,
	node string,
	name string,
	labels map[string]string,
) (*Probe, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	p := &Probe{
		clientset: clientset,
		config:    config,
		namespace: namespace,
		image:     probeImage(),
		node:      node,
		name:      name,
		labels:    labels,
	}
	pod := &corev1.Pod{
		Name: name, Namespace: namespace, Labels: labels,
		Spec: corev1.PodSpec{
			NodeName:                      node,
			HostPID:                       true,
			HostNetwork:                   true,
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptr.To[int64](0),
			Tolerations: []corev1.Toleration{
				{Operator: corev1.TolerationOpExists},
			},
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   p.image,
				Command: []string{"sh", "-c", "while true; do sleep 3600; done"},
				SecurityContext: &corev1.SecurityContext{
					Privileged: new(true),
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "host", MountPath: "/host"}},
			}},
			Volumes: []corev1.Volume{{
				Name:     "host",
				HostPath: &corev1.HostPathVolumeSource{Path: "/"},
			}},
		},
	}
	if _, err := clientset.CoreV1().
		Pods(namespace).
		Create(ctx, pod, metav1.CreateOptions{}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("creating probe pod %s/%s: %w", namespace, name, err)
	}
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			current, err := clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			switch current.Status.Phase {
			case corev1.PodRunning:
				return true, nil
			case corev1.PodFailed, corev1.PodSucceeded:
				return false, fmt.Errorf(
					"probe pod %s/%s ended in phase %s",
					namespace,
					name,
					current.Status.Phase,
				)
			default:
				return false, nil
			}
		})
	if err != nil {
		return nil, fmt.Errorf("waiting for probe pod %s/%s: %w", namespace, name, err)
	}
	return p, nil
}

// Delete removes the probe pod.
func (p *Probe) Delete(ctx context.Context) error {
	err := p.clientset.CoreV1().Pods(p.namespace).Delete(ctx, p.name, metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To[int64](0),
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// Run executes a command in the host's namespaces and returns its stdout. A
// non-zero exit returns an error carrying stderr.
func (p *Probe) Run(ctx context.Context, command ...string) (string, error) {
	args := append([]string{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--"}, command...)
	req := p.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(p.namespace).
		Name(p.name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "probe",
			Command:   args,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(p.config, "POST", req.URL())
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return stdout.String(), fmt.Errorf(
			"%s on %s: %w: %s", strings.Join(command, " "), p.node, err,
			strings.TrimSpace(stderr.String()),
		)
	}
	return stdout.String(), nil
}
