# Karpenter - Oxide Provider

## Getting Started

Both charts are published to ghcr.io alongside the controller image. Their chart version matches the
release version, and the controller image defaults to the matching tag, so no image values need to be
set.

### Install Karpenter CRDs (NodeClaim, NodePool, NodeOverlay, OxideNodeClass)

```
helm upgrade --install karpenter-crds oci://ghcr.io/oxidecomputer/karpenter/helm-charts/karpenter-crd
```

### Add the Oxide API Token Secret

The controller reads its Oxide credentials from the environment so the token is never baked into an
image or a Helm release.

```
kubectl create secret --namespace kube-system generic oxide-api-token --from-literal=token=${OXIDE_TOKEN}
```

### Install the Controller

```
helm upgrade --install karpenter oci://ghcr.io/oxidecomputer/karpenter/helm-charts/karpenter \
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
`oci://ghcr.io/oxidecomputer/karpenter/helm-charts` at the release version, and a GitHub release is
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
