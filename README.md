# karpenter-provider-oxide

This project is currently in public preview.

The Karpenter provider for Oxide enables you to manage Kubernetes nodes on Oxide. 
Karpenter scales up your nodes when needed and then back down when they're not.

## Installation

Install the CRDs and the Karpenter provider for Oxide from their GHCR OCI charts:

```console
helm upgrade --install karpenter-crds \
  oci://ghcr.io/oxidecomputer/helm-charts/karpenter-oxide-crd
```

```console
helm upgrade --install karpenter \
  --namespace kube-system \
  --set settings.oxide.host=${OXIDE_HOST} \
  --set settings.oxide.project=${OXIDE_PROJECT} \
  oci://ghcr.io/oxidecomputer/helm-charts/karpenter-provider-oxide
```

Karpenter uses an Oxide token secret to manage instances via the Oxide API:

```console
kubectl create secret generic oxide-api-token \
  --namespace kube-system \
  --from-literal=token=${OXIDE_TOKEN}
```

## Local simulation with KWOK

The [KWOK stack](kwok/README.md) combines the production controller-manager, a synthetic
Node registration bridge, and pinned KWOK in one Helm chart. It supports KinD or
an existing test cluster, plus a shareable bundle with local images. Supply your
own Omicron/Nexus endpoint, credentials, image and subnet.

## Example

To start scaling nodes, apply the following NodePool and OxideNodeClass in your K8s cluster:

```sh
export OXIDE_IMAGE_ID='your-image-uuid'
export OXIDE_PROJECT='your-project'
export OXIDE_VPC='your-vpc'

cat <<EOF | kubectl apply -f -
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.oxide.computer
        kind: OxideNodeClass
        name: default
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: [amd64]
        - key: kubernetes.io/os
          operator: In
          values: [linux]
---
apiVersion: karpenter.oxide.computer/v1alpha1
kind: OxideNodeClass
metadata:
  name: default
spec:
  storage:
    boot:
      size: 20Gi
  imageSelector:
    - id: ${OXIDE_IMAGE_ID}
  subnetSelector:
    - project: ${OXIDE_PROJECT}
      vpcName: ${OXIDE_VPC}
EOF
```

## Instance scheduling labels

Every generated instance type, NodeClaim, and Node has shape labels that can be used in
NodePool requirements:

| Label | Unit | Example for 4 CPU / 16 GiB |
| --- | --- | --- |
| `karpenter.oxide.computer/instance-cpu` | CPUs | `4` |
| `karpenter.oxide.computer/instance-memory` | MiB | `16384` |
| `karpenter.oxide.computer/instance-cpu-memory-ratio` | Reduced CPU:GiB ratio | `1-4` |
| `karpenter.oxide.computer/instance-cpu-platform` | Oxide CPU platform | `amd_milan` |

CPU and memory are numeric and support range operators. The ratio is reduced, so 8 CPU and
32 GiB is also `1-4`. Kubernetes label values cannot contain `:`, so the label-safe value
uses `-`: logical ratios `1:2`, `1:4`, `1:8`, `1:16`, and `1:32` are stored as `1-2`,
`1-4`, `1-8`, `1-16`, and `1-32`. Select ratio values with `In` or `NotIn`.

The CPU platform is one of `amd_milan`, `amd_turin`, or `amd_turin_v2` and defaults to
`amd_milan` when a NodePool does not require one. A platform is a minimum: the instance only
lands on sleds with every feature of that platform.
