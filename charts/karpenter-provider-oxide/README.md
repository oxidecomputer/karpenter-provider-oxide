# karpenter-provider-oxide

![Version: 0.0.1](https://img.shields.io/badge/Version-0.0.1-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.1](https://img.shields.io/badge/AppVersion-0.0.1-informational?style=flat-square)

A Helm chart for the Karpenter Oxide provider controller.

**Homepage:** <https://github.com/oxidecomputer/karpenter-provider-oxide>

## Installing the Chart

To install the chart with the release name `karpenter`:

```console
helm upgrade --install karpenter oci://ghcr.io/oxidecomputer/helm-charts/karpenter-provider-oxide \
  --namespace kube-system
```

The controller reads its Oxide credentials from a Secret in the release namespace, named
for the release (`settings.oxide.secretName`), which is not created by this chart:

```console
kubectl create secret --namespace kube-system generic karpenter \
  --from-literal=oxide-host=${OXIDE_HOST} \
  --from-literal=oxide-token=${OXIDE_TOKEN} \
  --from-literal=oxide-project=${OXIDE_PROJECT}
```

The CRDs live in the separate `karpenter-provider-oxide-crds` chart and must be installed first.

## Source Code

* <https://github.com/oxidecomputer/karpenter-provider-oxide>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| additionalAnnotations | object | `{}` | Additional annotations to add into metadata. |
| additionalClusterRoleRules | list | `[]` | Specifies additional rules for the core ClusterRole. |
| additionalLabels | object | `{}` | Additional labels to add into metadata. |
| affinity | object | `{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"karpenter.sh/nodepool","operator":"DoesNotExist"}]}]}},"podAntiAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":[{"topologyKey":"kubernetes.io/hostname"}]}}` | Affinity rules for scheduling the pod. |
| bootstrap | object | `{"kubeadm":{"enabled":false},"rke2":{"enabled":true}}` | Bootstrap provider configuration. Which bootstrap providers this install may operate. The provider itself is detected from the cluster; these only gate its RBAC. |
| bootstrap.kubeadm.enabled | bool | `false` | Enable RBAC for the kubeadm bootstrap provider. |
| bootstrap.rke2.enabled | bool | `true` | Enable RBAC for the RKE2 bootstrap provider. |
| controller.env | list | `[]` | Additional environment variables for the controller pod. |
| controller.envFrom | list | `[]` |  |
| controller.extraVolumeMounts | list | `[]` | Additional volumeMounts for the controller pod. |
| controller.healthProbe.port | int | `8081` | The container port to use for http health probe. |
| controller.image.digest | string | `""` | SHA256 digest of the controller image. |
| controller.image.repository | string | `"ghcr.io/oxidecomputer/karpenter-provider-oxide"` | Repository path to the controller image. |
| controller.image.tag | string | `""` | Tag of the controller image. |
| controller.metrics.port | int | `8080` | The container port to use for metrics. |
| controller.resources | object | `{}` | Resources for the controller pod. |
| dnsConfig | object | `{}` | Configure DNS Config for the pod. |
| dnsPolicy | string | `"Default"` | Configure the DNS Policy for the pod. |
| extraVolumes | list | `[]` | Additional volumes for the pod. |
| fullnameOverride | string | `""` | Overrides the chart's computed fullname. |
| hostNetwork | bool | `false` | Bind the pod to the host network. |
| imagePullPolicy | string | `"IfNotPresent"` | Image pull policy for Docker images. |
| imagePullSecrets | list | `[]` | Image pull secrets for Docker images. |
| kubeconfig.secretName | string | `""` | Secret holding a kubeconfig (key `value`) for the cluster to manage, when the controller runs outside it. |
| logLevel | string | `"info"` | Global log level, defaults to 'info'. |
| nameOverride | string | `""` | Overrides the chart's name. |
| nodeSelector | object | `{"kubernetes.io/os":"linux"}` | Node selectors to schedule the pod to nodes with labels. |
| podAnnotations | object | `{}` | Additional annotations for the pod. |
| podDisruptionBudget.maxUnavailable | int | `1` |  |
| podDisruptionBudget.name | string | `"karpenter"` |  |
| podLabels | object | `{}` | Additional labels for the pod. |
| podSecurityContext | object | `{"fsGroup":65536}` | SecurityContext for the pod. |
| priorityClassName | string | `"system-cluster-critical"` | PriorityClass name for the pod. |
| rbac.create | bool | `true` | Specifies if the ServiceAccount's roles and bindings should be created. |
| replicas | int | `1` | Number of replicas. |
| revisionHistoryLimit | int | `10` | The number of old ReplicaSets to retain to allow rollback. |
| serviceAccount.annotations | object | `{}` | Additional annotations for the ServiceAccount. |
| serviceAccount.create | bool | `true` | Specifies if a ServiceAccount should be created. |
| serviceAccount.name | string | `""` | The name of the ServiceAccount to use. |
| serviceMonitor.additionalLabels | object | `{}` | Additional labels for the ServiceMonitor. |
| serviceMonitor.enabled | bool | `false` | Specifies whether a ServiceMonitor should be created. |
| serviceMonitor.endpointConfig | object | `{}` | Endpoint configuration for the ServiceMonitor. |
| settings | object | `{"batchIdleDuration":"1s","batchMaxDuration":"10s","featureGates":{"nodeOverlay":false,"nodeRepair":false,"staticCapacity":false,"terminateFirstRepair":false},"instanceTypes":{"incrementPercent":10},"inventory":{"enabled":false},"minValuesPolicy":"Strict","nodeAttestor":{"url":""},"nodeRepair":{"tolerationDuration":""},"oxide":{"clusterID":"","secretName":""},"preferencePolicy":"Respect","reaper":{"quarantine":""}}` | Global settings to configure Karpenter. |
| settings.batchIdleDuration | string | `"1s"` | The maximum amount of time with no new pending pods that if exceeded ends the current batching window. |
| settings.batchMaxDuration | string | `"10s"` | The maximum length of a batch window. |
| settings.featureGates | object | `{"nodeOverlay":false,"nodeRepair":false,"staticCapacity":false,"terminateFirstRepair":false}` | Feature gate configuration values. |
| settings.featureGates.nodeOverlay | bool | `false` | nodeOverlay is ALPHA and is disabled by default. |
| settings.featureGates.nodeRepair | bool | `false` | nodeRepair is ALPHA and is disabled by default. |
| settings.featureGates.staticCapacity | bool | `false` | staticCapacity is ALPHA and is disabled by default. |
| settings.featureGates.terminateFirstRepair | bool | `false` | terminateFirstRepair terminates an unhealthy node before its replacement is ready when the replacement can't launch first. Requires nodeRepair. |
| settings.instanceTypes | object | `{"incrementPercent":10}` | Instance type catalog generation. |
| settings.instanceTypes.incrementPercent | int | `10` | Percentage increase between successive CPU and memory values in the generated catalog. |
| settings.inventory | object | `{"enabled":false}` | Sled inventory configuration. |
| settings.inventory.enabled | bool | `false` | Enable the sled inventory controllers. |
| settings.minValuesPolicy | string | `"Strict"` | How the Karpenter scheduler treats min values. |
| settings.nodeAttestor | object | `{"url":""}` | Node attestation configuration. EXPERIMENTAL and disabled by default. |
| settings.nodeAttestor.url | string | `""` | EXPERIMENTAL: URL of the cluster's oxide-node-attestor. Set, kubeadm nodes attest to it for the credential they join with rather than carry a bootstrap token, and their image must provide `oxide-node-join`. |
| settings.nodeRepair | object | `{"tolerationDuration":""}` | Node repair configuration. |
| settings.nodeRepair.tolerationDuration | string | `""` | How long a node may stay NotReady before it is rebooted. Defaults to 30m. |
| settings.oxide | object | `{"clusterID":"","secretName":""}` | Oxide API configuration. |
| settings.oxide.clusterID | string | `""` | Identity this cluster records on every Oxide resource it creates. |
| settings.oxide.secretName | string | `""` | Secret in the release namespace holding `oxide-host`, `oxide-token` and `oxide-project`. Defaults to the release's full name. |
| settings.preferencePolicy | string | `"Respect"` | How the Karpenter scheduler should treat preferences. |
| settings.reaper | object | `{"quarantine":""}` | Reaper configuration. |
| settings.reaper.quarantine | string | `""` | How long an unowned Oxide resource is left alone before it is deleted. Defaults to 5m. |
| strategy | object | `{"rollingUpdate":{"maxUnavailable":1}}` | Strategy for updating the pod. |
| terminationGracePeriodSeconds | string | `nil` | Override the default termination grace period for the pod. |
| tolerations | list | `[{"key":"CriticalAddonsOnly","operator":"Exists"}]` | Tolerations to allow the pod to be scheduled to nodes with taints. |
| topologySpreadConstraints | list | `[]` | Topology spread constraints for the pod. |

----------------------------------------------

Autogenerated from chart metadata using [helm-docs](https://github.com/norwoodj/helm-docs/).
