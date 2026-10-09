# Oxide Karpenter + KWOK

This stack runs the **production Oxide controller**, a separate registration bridge,
and upstream KWOK. Nexus remains responsible for instances, disks and placement.
The bridge only reads Nexus. It creates synthetic Kubernetes Nodes from persisted,
successfully launched NodeClaims; KWOK simulates kubelet readiness, leases and Pods.
The production launch/bootstrap/termination path is unchanged.

Omicron startup, its URL, credentials, image and subnet are supplied by you. No
Omicron deployment is included. Instances must reach `running` through the normal
API. Bootstrap is rendered normally but never executed by a simulated guest.

## Quick start from this repository

Requirements: Go matching `go.mod` (build only), Docker, kind **v0.32.0**, kubectl,
Helm (repository pins v4.2.4), jq, and the repository's `bin/ko` (`make tools`).
The pinned kind image supports both amd64 and arm64. Local builds use the host
architecture. The synthetic guest architecture is still the Oxide image's amd64.

```sh
make -C kwok cluster
export KUBE_CONTEXT=kind-oxide-kwok
export NAMESPACE=oxide-kwok

# Set the token without putting it in values.yaml or a shell command argument.
read -rs OXIDE_TOKEN; export OXIDE_TOKEN; echo
kubectl --context "$KUBE_CONTEXT" create namespace "$NAMESPACE"
printf '%s' "$OXIDE_TOKEN" | kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" \
  create secret generic oxide-api-token --from-file=token=/dev/stdin
unset OXIDE_TOKEN

cp kwok/examples/values.yaml /tmp/oxide-kwok-values.yaml
# Edit host/project. The URL must be reachable from controller Pods;
# localhost in a Pod is NOT the host's Omicron. Configure routing yourself.
export VALUES=/tmp/oxide-kwok-values.yaml
make -C kwok apply                     # build/load BOTH images; install the chart

export OXIDE_IMAGE_ID='<existing image UUID>'
export OXIDE_SUBNET_ID='<existing subnet UUID>'
make -C kwok fixtures                  # creates NodeClass and NodePool only
make -C kwok smoke                     # opt-in: exercises the configured Nexus
```

`OXIDE_HOST`, `OXIDE_PROJECT`, and optional `OXIDE_CLUSTER_ID` use their ordinary
production meanings. For Helm, configure them once under
`provider.settings.oxide.{host,project,clusterID}`; both processes receive the same
settings and existing Secret reference. The standalone bridge and audit binary
also accept the ordinary environment variables directly.

The example disables the boot-disk pool so a small test has no prewarmed disks to
account for. It sets `spec.server` explicitly and exercises kubeadm discovery and
join-token RBAC. `fixtures` reuses cloud image/subnet IDs; it never uploads or
deletes reusable cloud fixtures. Base workloads have no startup taints.

## Hand someone a release bundle

For a published release, the regular release pipeline builds both Linux image
architectures, packages the umbrella chart and attaches `oxide-kwok-VERSION.tar.gz`
and the Helm packages/checksums to the GitHub release. No source checkout or Go
installation is needed by the recipient.

```sh
# Maintainer: build the chart and distributable using published image tags.
make -C kwok bundle VERSION=1.2.3
# Or include unpublished, host-architecture images (Docker and ko required):
make -C kwok bundle-local VERSION=0.0.0-test
```

The default public bridge image name is the release destination; it is not
available until a release has actually been published. Use `apply` or
`bundle-local` for an unpublished revision.

Recipient workflow after extracting the bundle:

```sh
cd oxide-kwok-VERSION
shasum -a 256 -c SHA256SUMS
source versions.env
kind create cluster --name oxide-kwok --image "$KIND_IMAGE" \
  --config examples/kind.yaml --wait 120s
export KUBE_CONTEXT=kind-oxide-kwok NAMESPACE=oxide-kwok
# Create the token Secret as above; edit a copy of examples/values.yaml.
export VALUES=/absolute/path/to/my-values.yaml

# Only for a local-image bundle; recipient cluster architecture must match:
kind load image-archive --name oxide-kwok images.tar
./hack/install.sh --values images-values.yaml
# For published images instead:
# ./hack/install.sh

export OXIDE_IMAGE_ID='<image UUID>' OXIDE_SUBNET_ID='<subnet UUID>'
./hack/fixtures.sh
./hack/smoke.sh
./hack/diagnostics.sh
./hack/teardown.sh
./hack/audit.sh
./hack/uninstall.sh
```

A local-image bundle contains all three controller images and no cloud fixtures.
The initial kind node image and client tools must already exist or be pulled.
It is a portable test distribution, not a completely air-gapped Kubernetes installer.
The existing Secret is retained on uninstall.

## Existing Kubernetes context

Skip cluster creation/deletion. Set `KUBE_CONTEXT` explicitly, provide a registry
reachable by that cluster, and install the packaged chart:

```sh
export KUBE_CONTEXT=my-test-cluster NAMESPACE=oxide-kwok
export VALUES=/absolute/path/to/my-values.yaml
export CHART=/absolute/path/to/karpenter-provider-oxide-kwok-1.2.3.tgz
./kwok/hack/install.sh
```

The example assumes kubeadm `cluster-info` and `kubeadm-config` ConfigMaps. For
RKE2, configure the **existing production chart**'s `provider.bootstrap.rke2`,
disable `provider.bootstrap.kubeadm`, and use an ordinary RKE2 NodeClass instead
of the kubeadm fixture. The preflight script currently targets kubeadm; install
with `helm upgrade --install` directly for an intentionally different bootstrap.

Use a dedicated test cluster/project and one stack per cluster. Do not install a
second Karpenter or KWOK alongside an existing installation. All stack Pods have
required affinity away from simulated/Karpenter nodes; keep a real control-plane
or worker available. KWOK requires **both** management labels and only manages
Pods on those Nodes. The Helm release contains the production CRD chart; do not
uninstall it while any Karpenter/Oxide custom resources remain. `uninstall.sh`
checks this explicitly. CRD upgrades follow the existing production chart.

## Behavior and configuration

- Bridge replicas use the dedicated `oxide-kwok-bridge` leader-election Lease.
  KWOK is a single replica with a Recreate strategy.
- Node names and provider IDs retain `karpenter-<claim>` and `oxide://<UUID>`.
  Project UUID, full ownership marker and hostname must match before registration.
- Resource status is copied from the persisted claim, including boot-size-dependent
  ephemeral storage. A failed status write is retried after a process restart. A persisted initialization
  marker lets this finish even if Karpenter registers the Node before that write;
  completed resource status is never replayed.
- Already registered claims never get a replacement synthetic Node from the bridge.
  Karpenter retains all registration, repair, disruption and deletion decisions.
- The bridge never writes Ready. Scoped KWOK stages use strategic status patches
  that preserve capacity, allocatable and inventory conditions.
- `bridge.pollInterval` defaults to 5s, `apiTimeout` to 60s, `concurrency` to 2.
  API failures back off from 1s to 30s. `staleAfter` defaults to 1m, measured from
  the last successful observation persisted on the Node. A failed request and
  queue delay can extend the time before the bridge publishes Unknown.
- Running backends are Ready; confirmed stopped/failed/destroyed or missing
  backends are NotReady. Transitional/unrecognized states are Unknown. 401/403,
  network errors, generic 404s and server errors never mean a missing instance.
- Optional `bridge.removeStartupTaints: ["node.cilium.io/agent-not-ready:NoSchedule"]`
  simulates a missing CNI only **after registration** and only for exact taints in
  the claim's startup list. Permanent and unknown startup taints remain.
- Controller-runtime exposes reconciliation/error/duration metrics at bridge port
  8080; health probes use 8081. KWOK health/metrics use 10247. Cloud IDs are not
  used as metric label values.

See [chart defaults](../charts/karpenter-provider-oxide-kwok/values.yaml) for image
repositories/tags/digests, resources, probes, placement and pull-secret settings.
For a bundle, `helm show values ./karpenter-provider-oxide-kwok-*.tgz` shows the same defaults.

## Scale down and cleanup

```sh
make -C kwok scale-down   # remove workload/pool; wait for normal termination
make -C kwok audit        # read-only in-cluster Job checks owned instance/disk inventory
make -C kwok uninstall    # existing cluster: audit and check no remaining CRD instances
# OR, while the controllers are still installed:
make -C kwok delete-cluster # audit then delete only a kind cluster marked by `make cluster`
```

Do not uninstall the controllers first or remove finalizers to force cleanup.
A PDB, API outage or stuck disk should make cleanup fail visibly with controllers
still available. `audit` never treats an unsuccessful list as empty. The audit
checks all per-claim resources for this cluster identity in the configured
project; user-seeded images/subnets and pooled boot disks have separate ownership
and are not considered per-claim leaks. Remove any explicitly enabled pool
NodeClasses and wait for their normal cleanup before uninstalling.

## Validation and remaining integration work

Offline checks:

```sh
make -C kwok verify
# Runs temporary etcd and kube-apiserver, never the active kubeconfig or Nexus:
KUBEBUILDER_ASSETS=/path/to/envtest/binaries make -C kwok test-api
# Optional: exercise actual KWOK v0.8.0 against that temporary API server too:
KWOK_BINARY=/path/to/kwok KUBEBUILDER_ASSETS=/path/to/envtest/binaries \
  go test ./kwok/controllers/node -run TestAPIServerRegistration -count=1
```

Pinned inputs: KWOK v0.8.0; kind v0.32.0 with Kubernetes v1.36.1 (exact image digest
in `versions.env`). Local API-server testing uses Kubernetes v1.36.2. Upstream Pod
stages are vendored verbatim from KWOK v0.8.0 and licensed under Apache 2.0; see
`charts/karpenter-provider-oxide-kwok/files/KWOK-LICENSE`. Custom Node stages replace
the upstream default Ready stage entirely. No installation-time fetches or Stage
CRDs are needed. Reference sources: [KWOK release](https://github.com/kubernetes-sigs/kwok/releases/tag/v0.8.0),
[kind release](https://github.com/kubernetes-sigs/kind/releases/tag/v0.32.0).

**No live Omicron or real Oxide validation was performed during implementation.**
Run `smoke`, scale-down and audit after configuring your backend. Full end-to-end
PDB, expiry/drift, pooled storage, quota, placement, repair/reaper and scale-limit
validation still requires that backend. The supplied smoke checks launches,
registration/initialization, scheduled Pods, resource parity and bridge restart.
It leaves workloads running for inspection; scale-down and audit are explicit.
No measured scale/runtime claim is made yet.

This simulates control-plane behavior. It does not test guest networking, CNI,
cloud-init, filesystems, actual container execution, CCM guest integration or
hardware performance. Against real Oxide the unchanged provider creates real
resources; use a dedicated project and an image that will not also join a kubelet.
