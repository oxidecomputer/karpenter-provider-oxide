# Karpenter - Oxide Provider

## Getting Started

Both charts are published to ghcr.io alongside the controller image. Their chart version matches the
release version, and the controller image defaults to the matching tag, so no image values need to be
set.

### Install Karpenter CRDs (NodeClaim, NodePool, NodeOverlay, OxideNodeClass)

```
helm upgrade --install karpenter-crds oci://ghcr.io/oxidecomputer/helm-charts/karpenter-crd
```

### Add the Oxide API Token Secret

The controller reads its Oxide credentials from the environment so the token is never baked into an
image or a Helm release.

```
kubectl create secret --namespace kube-system generic oxide-api-token --from-literal=token=${OXIDE_TOKEN}
```

### Install the Controller

```
helm upgrade --install karpenter oci://ghcr.io/oxidecomputer/helm-charts/karpenter \
  --namespace kube-system \
  --set settings.oxide.host=${OXIDE_HOST} \
  --set settings.oxide.project=${OXIDE_PROJECT}
```

To install from a checkout instead, point Helm at `charts/karpenter-crd` and `charts/karpenter`. The
chart versions in the repo are placeholders, so `--set controller.image.repository`/`.tag` are needed
to select an image.

## Bootstrap Providers

An `OxideNodeClass` selects exactly one bootstrap provider under `spec.bootstrap`. Settings that
every provider understands — `spec.server` and `spec.kubeletArgs` — live at the top level of the
spec instead, and `spec.server` may be left empty to have the kube-apiserver address discovered from
the `default/kubernetes` EndpointSlice.

Enable RBAC only for the providers actually in use. Each provider gets its own namespaced Role, so a
disabled provider's permissions are never created:

```
--set bootstrap.rke2.enabled=true --set bootstrap.rke2.tokenSecret.name=rancher-agent-token
--set bootstrap.kubeadm.enabled=true
```

### RKE2

Nodes join through the RKE2 supervisor on port 9345 using a long-lived agent token that you create
and reference from the nodeclass. The controller is granted `get` on that one Secret by name, so
`bootstrap.rke2.tokenSecret.name` in the chart values must match `spec.bootstrap.rke2.tokenRef`.

```
kubectl create secret --namespace kube-system generic rancher-agent-token --from-literal=token=${RKE_AGENT_TOKEN}
```

See `examples/oxidenodeclass.yaml`.

### kubeadm

Nodes join through the kube-apiserver on port 6443. There is no long-lived credential to configure:

- Discovery information is read from the `kube-public/cluster-info` ConfigMap, which kubeadm
  publishes for exactly this purpose. The cluster CA is pinned by a `sha256` hash of its
  SubjectPublicKeyInfo so a joining node verifies the control plane it is told to trust.
- A bootstrap token is minted per NodeClaim as a Secret in `kube-system`, valid for 15 minutes by
  default (`spec.bootstrap.kubeadm.tokenTTL`), and handed to exactly one node through user-data. The
  token is granted only `usage-bootstrap-authentication`; signing is unnecessary because the CA is
  pinned by hash. Expired tokens are garbage collected by the control plane's `tokencleaner`
  controller, so the controller needs neither read nor delete access to `kube-system` Secrets.

Set `--set bootstrap.kubeadm.configVersion=kubeadm.k8s.io/v1beta3` on clusters older than Kubernetes
1.31. See `examples/oxidenodeclass-kubeadm.yaml`.

### Create a NodePool

```
cat <<EOF | envsubst | kubectl apply -f -
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  template:
    spec:
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
        - key: kubernetes.io/os
          operator: In
          values: ["linux"]
      expireAfter: 720h
      nodeClassRef:
        group: karpenter.oxide.computer
        kind: OxideNodeClass
        name: default
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 5s
---
apiVersion: karpenter.oxide.computer/v1alpha1
kind: OxideNodeClass
metadata:
  name: default
spec:
  kubeletArgs:
    - cloud-provider=external
  bootstrap:
    rke2:
      tokenRef:
        name: rancher-agent-token
        namespace: kube-system
        key: token
EOF
```

If your CNI or cloud controller manager taints a node while it starts up, list that taint under
`spec.template.spec.startupTaints`. Karpenter discounts a node's startup taints while it is
registered but not yet initialized; a taint it does not know about instead makes the node look
unusable, and Karpenter launches a second node for pods the first one was already going to take.
It already knows about `node.kubernetes.io/not-ready`,
`node.cloudprovider.kubernetes.io/uninitialized:NoSchedule`, and `karpenter.sh/unregistered`,
matching by key and effect only. See `examples/nodepool.yaml`.

Startup taints are also applied to the node when it registers, so only list taints that something
removes once the node is ready. Check what your cluster applies with:

```
kubectl get nodes -w -o jsonpath='{.metadata.name}{"\t"}{.spec.taints}{"\n"}'
```

### Selecting an Image

`spec.imageSelector` is a list of selectors OR'd together; the terms within one selector are AND'd.
An `id` selects one image directly, while `name`, `os`, and `version` select from the images in
`project` (or the silo images not linked to a project, when `project` is unset). When more than one
image matches, nodes launch with the most recently uploaded one.

`name` is matched exactly unless it contains a `*`, where each `*` stands for any run of
characters, including none. `rke2-ubuntu-*` and `rke2-*` both select `rke2-ubuntu-1780929039`, as
does `*-ubuntu-*`. A wildcard does not have to fall on a `-` boundary, so `ubuntu*` selects
`ubuntu-2404`.

Oxide image versions are free-form, so `version` is matched literally unless it contains a `*`. A
`*` matches whole segments, split on `.`, `-`, `_`, or whitespace, with a leading `v` ignored on
both sides. In the last position it matches one or more remaining segments; anywhere else it
matches exactly one:

| `version`         | Matches                        | Does not match           |
| ----------------- | ------------------------------ | ------------------------ |
| `1.2.3`           | `1.2.3`                        | `1.2.30`, `v1.2.3`       |
| `1.2.*`           | `1.2.3`, `1.2.11`              | `1.2`, `1.3.0`, `1.20`   |
| `1.*`             | `1.4`, `1.4.2`                 | `1`, `10.1`              |
| `24.04 *`         | `24.04 2026-05-09`             | `24.10 2026-05-09`       |
| `*`               | any version                    | an image with no version |

Because a `*` cannot consume part of a segment, `1.2.*` will never drift onto `1.20`.

```
kubectl apply -f - <<EOF
apiVersion: karpenter.oxide.computer/v1alpha1
kind: OxideNodeClass
metadata:
  name: default
spec:
  imageSelector:
    # The newest 1.2.x image in the "prod" project, whatever patch that is today.
    - os: ubuntu
      version: 1.2.*
      project: prod
EOF
```

`status.images` reports the images the selectors resolved to, newest first. Nodes always launch
with the first entry; up to two runners-up are listed after it so a wildcard's selection can be
checked without re-deriving it:

```
kubectl get oxidenodeclass default -o jsonpath='{.status.images}' | jq
```

When nothing matches, the nodeclass reports `ImagesResolved=False` and is held un-Ready, so
Karpenter will not launch against it. The condition message repeats the selectors that were tried:

```
$ kubectl get oxidenodeclass default -o jsonpath='{.status.conditions}' | jq -r '.[] | select(.type=="ImagesResolved") | .message'
no images matched {os=ubuntu,version=1.2.*,project=prod}
```

### Create a Deployment

```
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: inflate
spec:
  replicas: 0
  selector:
    matchLabels:
      app: inflate
  template:
    metadata:
      labels:
        app: inflate
    spec:
      terminationGracePeriodSeconds: 0
      securityContext:
        runAsUser: 1000
        runAsGroup: 3000
        fsGroup: 2000
      containers:
      - name: inflate
        image: registry.k8s.io/pause:3.10.2
        resources:
          requests:
            cpu: 1
            memory: 256Mi
        securityContext:
          allowPrivilegeEscalation: false
EOF
```

### Scale the Deployment

```
kubectl scale deploy inflate --replicas 50
```

### Create a Deployment w/ Hostname Pod Anti-Affinity

```
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: inflate-anti-affinity
spec:
  replicas: 0
  selector:
    matchLabels:
      app: inflate-anti-affinity
  template:
    metadata:
      labels:
        app: inflate-anti-affinity
    spec:
      affinity:
        podAntiAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - labelSelector:
                matchLabels:
                  app: inflate-anti-affinity
              topologyKey: kubernetes.io/hostname
      terminationGracePeriodSeconds: 0
      securityContext:
        runAsUser: 1000
        runAsGroup: 3000
        fsGroup: 2000
      containers:
      - name: inflate
        image: registry.k8s.io/pause:3.10.2
        resources:
          requests:
            cpu: 1
            memory: 256Mi
        securityContext:
          allowPrivilegeEscalation: false
EOF
```

## Silo Quota

A silo has a quota — vCPUs, memory and storage it is entitled to — separate from whether the rack has
room. Karpenter reads it from `/v1/utilization` and only offers the scheduler instance types the silo
can actually launch.

This is on by default and needs no extra permissions: a silo can read its own utilization, so any
token that can create instances can read this. Turn it off with
`--set settings.quota.enabled=false`.

Without it, the scheduler is offered every shape up to a whole sled. A silo entitled to 16 vCPUs would
have Karpenter confidently pick a 256 vCPU node, create a NodeClaim, fail at the API, delete the
NodeClaim, and try again — burning a full launch attempt on a decision that was knowable up front.

### Why the Quota Also Fixes the Error Message

The Oxide API reports a silo quota with no room and a rack with no sled available **identically**,
both as `InsufficientCapacity`. There is no error code that distinguishes them. So having the quota on
hand is what makes a refusal explainable: given the shape that was refused and the quota headroom at
the time, the provider decides which limit was actually binding and says so.

A quota refusal reads like this, on the NodeClaim and in the events:

```
the silo's cpus quota has no room for this node: it needs 32 vCPUs, and 48 vCPUs of the
64 vCPUs quota is already provisioned, leaving 16 vCPUs
```

versus a rack refusal, which stays as-is: the rack has no sled with room. The distinction matters
because they need different responses — a quota needs an administrator, a full rack needs waiting or
smaller pods.

### Storage Is Checked Later Than CPU and Memory

CPU and memory are properties of the instance type, so they are folded into offerings and the scheduler
never picks an oversized shape. Storage is not: boot disk size comes from `spec.bootDiskSize` on the
OxideNodeClass, so two nodes of identical shape can consume different storage quota. It is therefore
checked at launch time, immediately before the instance is created, and produces the same style of
error:

```
the silo's storage quota has no room for this node: it needs 200 GiB, and 900 GiB of the
1024 GiB quota is already provisioned, leaving 124 GiB
```

Because a storage quota refuses every shape equally, this does **not** mark the shape unavailable —
there is no smaller instance type that would help, and saying otherwise about the shape would be
untrue.

### Interaction With the Unavailable Cache

The cache of recently-refused shapes now records *why* each shape is out, `InsufficientCapacity` or
`QuotaExceeded`. Both cover every shape at least as large in both dimensions, since a limit that
refuses 8 vCPUs and 16 GiB also refuses 16 vCPUs and 32 GiB. When both reasons cover a shape, quota is
reported: rack capacity is transient and Karpenter retries into it, whereas a quota keeps refusing
until somebody changes it.

Quota-derived unavailability is not only reactive. It is recomputed from each utilization read, so
raising a quota takes effect on the next refresh (`settings.quota.refreshPeriod`, default `1m`) rather
than waiting out a cache TTL.

### Failure Behavior

An unreadable quota constrains nothing. If `/v1/utilization` fails, the controller logs it once, every
instance type stays available, and a launch that exceeds the quota is refused by the API and reported
as an ordinary capacity error. That is the same behavior as `settings.quota.enabled=false`, which is
the point: a failed read must not stop a cluster from scaling.

The quota is a filter, not a lock. Concurrent launches all read the same snapshot, so a burst can still
overshoot and have the API refuse the tail of it. Those refusals are diagnosed against the quota,
marked, and trigger a re-read, so the cluster converges rather than looping.

## Sled Inventory (Optional)

Karpenter asks the Oxide API for an instance of a given size; the provision saga decides which
physical sled has room for it. Nothing in the provider needs to know which sled that was, so by
default it never asks.

Enabling sled inventory makes it ask, and records the answer on each Node and NodeClaim:

```sh
helm upgrade --install karpenter oci://ghcr.io/oxidecomputer/helm-charts/karpenter \
  --namespace kube-system \
  --reuse-values \
  --set settings.inventory.enabled=true
```

This is opt-in because of what it costs in permissions. The instance-to-sled mapping only exists on
the fleet-scoped system API (`/v1/system/hardware/sleds`), which is a far broader grant than creating
instances in one project. If you would rather not give Karpenter fleet read access, leave this off:
it is a supported configuration, not a degraded one.

It is also non-load-bearing by construction. Nothing else in the provider reads what it writes. If
inventory is enabled against a token that cannot reach the system API, the controller logs the
failure once, applies no labels, and node provisioning proceeds exactly as it would with inventory
disabled.

### What Gets Applied

Labels, on the Node:

| Label | Example | Meaning |
| --- | --- | --- |
| `karpenter.oxide.computer/sled-id` | `dd83e6bd-…` | The sled the node's instance is running on |
| `karpenter.oxide.computer/rack-id` | `a3f2b81c-…` | The rack the sled is attached to |
| `karpenter.oxide.computer/sled-serial` | `BRM42220004` | Baseboard serial of the sled |
| `karpenter.oxide.computer/sled-state` | `active` | Sled lifecycle state: `active` or `decommissioned` |
| `karpenter.oxide.computer/sled-policy` | `in_service` | Operator policy: `in_service` or `expunged` |
| `karpenter.oxide.computer/sled-provision-policy` | `provisionable` | Whether the sled accepts new resources |

Annotations, on both the Node and the NodeClaim: baseboard part and revision, the sled's usable
hardware threads and physical RAM, and `sled-observed-at`, the time of the API read the placement
came from.

A `SledUnhealthy` status condition, on the Node. `True` means the sled is being taken out of service,
with a reason of `SledExpunged` or `SledNonProvisionable`; `False` means it is in service. This is a
condition rather than a label because conditions are the only thing Karpenter's node repair can watch
— see below.

The labels go on the Node rather than the NodeClaim. A NodeClaim's labels are the scheduling contract
it was provisioned against, and placement is not part of that contract — it is discovered afterwards.
Keeping it out of the NodeClaim's labels is what makes it impossible for these to perturb drift or
disruption decisions.

### Observability

Correlate node-level symptoms with the hardware underneath them. With `sled-serial` on the node, a
metrics pipeline that already scrapes nodes can group by sled, and a support case has the serial to
hand. Find what is sharing a sled, or what is on hardware that is going away:

```sh
kubectl get nodes -L karpenter.oxide.computer/sled-serial
kubectl get nodes -l 'karpenter.oxide.computer/sled-policy=expunged'
kubectl get nodes -l 'karpenter.oxide.computer/sled-provision-policy=non_provisionable'
```

Note that `sled-id` is **not** useful as a pod topology key in the way a cloud zone would be. Oxide
places instances; there is no way to ask for one on a particular sled. So a topology spread over
`sled-id` can observe an imbalance but Karpenter cannot act on it — the replacement is just as likely
to land on the same sled. Use Oxide anti-affinity groups for spread, and treat these labels as
descriptive.

### Node Repair

If sled inventory and Karpenter's `nodeRepair` feature gate are both enabled, nodes on a sled that is
going out of service are drained and replaced:

```sh
helm upgrade --install karpenter oci://ghcr.io/oxidecomputer/helm-charts/karpenter \
  --namespace kube-system \
  --reuse-values \
  --set settings.inventory.enabled=true \
  --set settings.featureGates.nodeRepair=true
```

Both are required. With inventory off, the provider advertises no repair policies at all and core does
not even register its health controller. With the gate off, the `SledUnhealthy` condition is still
recorded but nothing acts on it.

The sequence is: the inventory controller sets `SledUnhealthy=True`; core's health controller waits
`settings.inventory.repairTolerationDuration` (default `30m`) from the condition's transition time,
then deletes the NodeClaim; termination drains the node; the pods go back to the scheduler and
Karpenter provisions replacement capacity.

Replacement lands elsewhere for the reason that makes this work without sled targeting: a sled that is
expunged or `non_provisionable` will not accept new placements, so the replacement instance
necessarily goes to a different sled. Nothing has to request that.

Two things to know before enabling it:

**Repair drains forcefully.** This is core Karpenter behavior, not something this provider chooses.
Once the toleration duration elapses, the NodeClaim is annotated with a termination timestamp of *now*,
which means pods are force-evicted past their `terminationGracePeriodSeconds` and PodDisruptionBudgets
and `karpenter.sh/do-not-disrupt` no longer hold. If you want the sled health signal without that, set
`settings.inventory.repairTolerationDuration=0` — the condition is still recorded and no repair policy
is advertised.

**A sled outage can exceed core's guardrail.** Core blocks repair while more than 20% of a NodePool's
nodes are unhealthy, emitting a `NodeRepairBlocked` event instead. A single sled can easily host more
than 20% of a small NodePool's nodes, in which case expunging it will *not* trigger automatic
replacement and you will need to drain it yourself. That guardrail is deliberate and this provider
does not override it.

The condition is only ever derived from a placement actually read from the API. A node whose sled
cannot be resolved — because the system API is unreachable, or the snapshot has not caught up with a
new instance — is never marked unhealthy, so a broken or unauthorized system API cannot cause
replacements. An unrecognized policy value reads as healthy for the same reason.

### Cost

Building the mapping takes one API call per sled in the rack, plus one, since there is no endpoint
that resolves a single instance to its sled. It is fetched into a shared snapshot on a schedule
(`settings.inventory.refreshPeriod`, default `5m`) rather than per node, and on-demand refreshes for
newly launched nodes are rate limited to a quarter of that period. Lowering `refreshPeriod` raises
fleet API load proportionally.

## Contributing

### Tooling

Run the following to download the tools required for development into `bin/`:

```
make tools
```

Tool versions are pinned in `tools/go.mod`, which is kept separate from the controller's `go.mod` so
their dependencies never enter the controller's module graph.

### Development

Generated artifacts — deepcopy code, the OxideNodeClass CRD, the vendored `karpenter.sh` CRDs, and the
chart READMEs — are produced by:

```
make generate
```

To run the controller against the cluster in your current kube context, or to build the image with
[ko](https://ko.build) and deploy it there:

```
make run
make apply
```

To see all available recipes:

```
make help
```

### Before Opening a PR

```
make precommit
```

This runs the same steps CI does (`make ci-verify`): fixes and lints, generates, tests, and builds a
local snapshot release. CI additionally fails if any of those steps leave the working tree dirty.

## Release

Tag the commit you are releasing with the semver release version (i.e. v1.0.9). A GitHub Actions
workflow will kick off that performs the release using [goreleaser](https://goreleaser.com): multi-arch
images are pushed to `ghcr.io/oxidecomputer/karpenter`, both charts are pushed to
`oci://ghcr.io/oxidecomputer/helm-charts` at the release version, and a GitHub release is
created.

```
git tag vX.Y.Z
git push origin vX.Y.Z
```

## License

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
