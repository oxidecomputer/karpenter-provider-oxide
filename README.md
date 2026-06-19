# Karpenter - Oxide Provider

## Getting Started

### Clone the repo

```
git clone 
```

### Install Karpenter CRDs (NodeClaim, NodePool, NodeOverlay, OxideNodeClass)

```
helm upgrade --install karpenter-crds charts/karpenter-crd
```

### Add the Oxide API Token Secret

The controller reads its Oxide credentials from the environment so the token is never baked into an
image or a Helm release.

```
kubectl create secret --namespace kube-system generic oxide-api-token --from-literal=token=${OXIDE_TOKEN}
```

### Install the Controller

```
helm upgrade --install karpenter charts/karpenter --namespace kube-system \
  --set settings.oxide.host=${OXIDE_HOST} \
  --set settings.oxide.project=${OXIDE_PROJECT} \
  --set controller.image.repository=${IMAGE_REPOSITORY}
```

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