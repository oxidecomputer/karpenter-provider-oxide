# karpenter-provider-oxide-kwok

![Version: 0.0.1](https://img.shields.io/badge/Version-0.0.1-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.1](https://img.shields.io/badge/AppVersion-0.0.1-informational?style=flat-square)

Production Oxide Karpenter with a synthetic kubelet bridge and pinned KWOK.

## Requirements

| Repository | Name | Version |
|------------|------|---------|
| file://../karpenter-provider-oxide-crds | karpenter-provider-oxide-crds | 0.0.1 |
| file://../karpenter-provider-oxide | provider(karpenter-provider-oxide) | 0.0.1 |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| bridge.apiTimeout | string | `"60s"` |  |
| bridge.concurrency | int | `2` |  |
| bridge.image.digest | string | `""` |  |
| bridge.image.pullPolicy | string | `"IfNotPresent"` |  |
| bridge.image.repository | string | `"ghcr.io/oxidecomputer/karpenter-provider-oxide-kwok"` |  |
| bridge.image.tag | string | `""` |  |
| bridge.pollInterval | string | `"5s"` |  |
| bridge.removeStartupTaints | list | `[]` |  |
| bridge.replicas | int | `1` |  |
| bridge.resources.limits.memory | string | `"256Mi"` |  |
| bridge.resources.requests.cpu | string | `"100m"` |  |
| bridge.resources.requests.memory | string | `"128Mi"` |  |
| bridge.staleAfter | string | `"1m"` |  |
| imagePullSecrets | list | `[]` |  |
| kwok.image | string | `"registry.k8s.io/kwok/kwok:v0.8.0@sha256:6d25aa8fbdfe78845423160bf125b5513f9522e2770981f0945c2a250c2b26f0"` |  |
| kwok.imagePullPolicy | string | `"IfNotPresent"` |  |
| kwok.resources.limits.memory | string | `"512Mi"` |  |
| kwok.resources.requests.cpu | string | `"100m"` |  |
| kwok.resources.requests.memory | string | `"128Mi"` |  |
| nodeSelector."kubernetes.io/os" | string | `"linux"` |  |
| podAnnotations | object | `{}` |  |
| provider.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key | string | `"kwok.x-k8s.io/node"` |  |
| provider.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator | string | `"DoesNotExist"` |  |
| provider.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[1].key | string | `"karpenter.sh/nodepool"` |  |
| provider.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[1].operator | string | `"DoesNotExist"` |  |
| provider.affinity.podAntiAffinity | object | `{}` |  |
| provider.bootstrap.kubeadm.enabled | bool | `true` |  |
| provider.bootstrap.rke2.enabled | bool | `false` |  |
| provider.controller.resources.limits.memory | string | `"1Gi"` |  |
| provider.controller.resources.requests.cpu | string | `"250m"` |  |
| provider.controller.resources.requests.memory | string | `"512Mi"` |  |
| provider.replicas | int | `1` |  |
| provider.settings.inventory.enabled | bool | `true` |  |
| provider.settings.oxide.clusterID | string | `""` |  |
| provider.settings.oxide.host | string | `""` |  |
| provider.settings.oxide.project | string | `""` |  |
| provider.settings.oxide.tokenSecret.key | string | `"token"` |  |
| provider.settings.oxide.tokenSecret.name | string | `"oxide-api-token"` |  |
| provider.tolerations[0].effect | string | `"NoSchedule"` |  |
| provider.tolerations[0].key | string | `"node-role.kubernetes.io/control-plane"` |  |
| provider.tolerations[0].operator | string | `"Exists"` |  |
| provider.tolerations[1].key | string | `"CriticalAddonsOnly"` |  |
| provider.tolerations[1].operator | string | `"Exists"` |  |
| provider.topologySpreadConstraints | list | `[]` |  |
| tolerations[0].effect | string | `"NoSchedule"` |  |
| tolerations[0].key | string | `"node-role.kubernetes.io/control-plane"` |  |
| tolerations[0].operator | string | `"Exists"` |  |
| tolerations[1].key | string | `"CriticalAddonsOnly"` |  |
| tolerations[1].operator | string | `"Exists"` |  |

