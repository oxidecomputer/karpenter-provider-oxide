# oxide-karpenter

The Karpenter provider for Oxide enables you to manage Kubernetes nodes on Oxide. 
Karpenter scales up your nodes when needed and then back down when they're not.

## Installation

To install the Karpenter provider for Oxide, use helm:

```
helm upgrade --install karpenter \
  --namespace kube-system \
  --set settings.oxide.host=${OXIDE_HOST} \
  --set settings.oxide.project=${OXIDE_PROJECT} \
  oci://ghcr.io/oxidecomputer/helm-charts/karpenter-provider-oxide
```

Karpenter uses an Oxide token secret to managed instances via the Oxide API:

```console
kubectl create secret --namespace kube-system generic oxide-api-token --from-literal=token=${OXIDE_TOKEN}
```

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
  kubeletArgs:
    - cloud-provider=external
  bootstrap:
    kubeadm: {}
  imageSelector:
    - id: ${OXIDE_IMAGE_ID}
  subnetSelector:
    - project: ${OXIDE_PROJECT}
      vpcName: ${OXIDE_VPC}
EOF
```
