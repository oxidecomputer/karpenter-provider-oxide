# karpenter

![Version: 0.0.1](https://img.shields.io/badge/Version-0.0.1-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.1](https://img.shields.io/badge/AppVersion-0.0.1-informational?style=flat-square)

A Helm chart for the Karpenter Oxide provider controller.

**Homepage:** <https://github.com/oxidecomputer/karpenter>

## Installing the Chart

To install the chart with the release name `karpenter`:

```console
helm upgrade --install karpenter oci://ghcr.io/oxidecomputer/helm-charts/karpenter \
  --namespace kube-system \
  --set settings.oxide.host=${OXIDE_HOST} \
  --set settings.oxide.project=${OXIDE_PROJECT}
```

The controller reads its Oxide API token from a Secret in the release namespace
(`settings.oxide.tokenSecret`), which is not created by this chart:

```console
kubectl create secret --namespace kube-system generic oxide-api-token --from-literal=token=${OXIDE_TOKEN}
```

The CRDs live in the separate `karpenter-crd` chart and must be installed first.

## Source Code

* <https://github.com/oxidecomputer/karpenter>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| additionalAnnotations | object | `{}` | Additional annotations to add into metadata. |
| additionalClusterRoleRules | list | `[]` | Specifies additional rules for the core ClusterRole. |
| additionalLabels | object | `{}` | Additional labels to add into metadata. |
| affinity | object | `{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"karpenter.sh/nodepool","operator":"DoesNotExist"}]}]}},"podAntiAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":[{"topologyKey":"kubernetes.io/hostname"}]}}` | Affinity rules for scheduling the pod. If an explicit label selector is not provided for pod affinity or pod anti-affinity one will be created from the pod selector labels. |
| bootstrap | object | `{"kubeadm":{"enabled":false},"rke2":{"enabled":true,"tokenSecret":{"name":"rancher-agent-token","namespace":""}}}` | Bootstrap provider configuration. Only enable the providers actually in use: each one is granted its own namespaced Role, so a disabled provider's permissions are never created at all. |
| bootstrap.kubeadm.enabled | bool | `false` | Enable RBAC for the kubeadm bootstrap provider. This grants get on the kube-public/cluster-info ConfigMap and create (but not read) on kube-system Secrets, so that short-lived bootstrap tokens can be minted per node. |
| bootstrap.rke2.enabled | bool | `true` | Enable RBAC for the RKE2 bootstrap provider. |
| bootstrap.rke2.tokenSecret | object | `{"name":"rancher-agent-token","namespace":""}` | The Secret holding the RKE2 agent join token. The controller is granted get on this one Secret by name, so this must match spec.bootstrap.rke2.tokenRef on the OxideNodeClass. |
| bootstrap.rke2.tokenSecret.name | string | `"rancher-agent-token"` | Name of the token Secret. |
| bootstrap.rke2.tokenSecret.namespace | string | `""` | Namespace of the token Secret. Defaults to the release namespace. |
| controller.env | list | `[]` | Additional environment variables for the controller pod. |
| controller.envFrom | list | `[]` |  |
| controller.extraVolumeMounts | list | `[]` | Additional volumeMounts for the controller pod. |
| controller.healthProbe.port | int | `8081` | The container port to use for http health probe. |
| controller.image.digest | string | `""` | SHA256 digest of the controller image. |
| controller.image.repository | string | `"ghcr.io/oxidecomputer/karpenter"` | Repository path to the controller image. |
| controller.image.tag | string | `""` | Tag of the controller image. Defaults to the chart's appVersion. |
| controller.metrics.port | int | `8080` | The container port to use for metrics. |
| controller.resources | object | `{}` | Resources for the controller pod. |
| dnsConfig | object | `{}` | Configure DNS Config for the pod. |
| dnsPolicy | string | `"Default"` | Configure the DNS Policy for the pod. |
| extraVolumes | list | `[]` | Additional volumes for the pod. |
| fullnameOverride | string | `""` | Overrides the chart's computed fullname. |
| hostNetwork | bool | `false` | Bind the pod to the host network. This is required when using a custom CNI. |
| imagePullPolicy | string | `"IfNotPresent"` | Image pull policy for Docker images. |
| imagePullSecrets | list | `[]` | Image pull secrets for Docker images. |
| logLevel | string | `"info"` | Global log level, defaults to 'info'. |
| nameOverride | string | `""` | Overrides the chart's name. |
| nodeSelector | object | `{"kubernetes.io/os":"linux"}` | Node selectors to schedule the pod to nodes with labels. |
| podAnnotations | object | `{}` | Additional annotations for the pod. |
| podDisruptionBudget.maxUnavailable | int | `1` |  |
| podDisruptionBudget.name | string | `"karpenter"` |  |
| podLabels | object | `{}` | Additional labels for the pod. |
| podSecurityContext | object | `{"fsGroup":65536}` | SecurityContext for the pod. |
| priorityClassName | string | `"system-cluster-critical"` | PriorityClass name for the pod. |
| replicas | int | `1` | Number of replicas. |
| revisionHistoryLimit | int | `10` | The number of old ReplicaSets to retain to allow rollback. |
| serviceAccount.annotations | object | `{}` | Additional annotations for the ServiceAccount. |
| serviceAccount.create | bool | `true` | Specifies if a ServiceAccount should be created. |
| serviceAccount.name | string | `""` | The name of the ServiceAccount to use. If not set and create is true, a name is generated using the fullname template. |
| serviceMonitor.additionalLabels | object | `{}` | Additional labels for the ServiceMonitor. |
| serviceMonitor.enabled | bool | `false` | Specifies whether a ServiceMonitor should be created. |
| serviceMonitor.endpointConfig | object | `{}` | Endpoint configuration for the ServiceMonitor. |
| settings | object | `{"batchIdleDuration":"1s","batchMaxDuration":"10s","featureGates":{"nodeOverlay":false,"nodeRepair":false,"spotToSpotConsolidation":false,"staticCapacity":false},"inventory":{"enabled":false,"refreshPeriod":"5m","repairTolerationDuration":"30m"},"minValuesPolicy":"Strict","oxide":{"host":"","project":"","tokenSecret":{"key":"token","name":"oxide-api-token"}},"preferencePolicy":"Respect"}` | Global settings to configure Karpenter. |
| settings.batchIdleDuration | string | `"1s"` | The maximum amount of time with no new pending pods that if exceeded ends the current batching window. |
| settings.batchMaxDuration | string | `"10s"` | The maximum length of a batch window. The longer this is, the more pods we can consider for provisioning at one time which usually results in fewer but larger nodes. |
| settings.featureGates | object | `{"nodeOverlay":false,"nodeRepair":false,"spotToSpotConsolidation":false,"staticCapacity":false}` | Feature gate configuration values. |
| settings.featureGates.nodeOverlay | bool | `false` | nodeOverlay is ALPHA and is disabled by default. |
| settings.featureGates.nodeRepair | bool | `false` | nodeRepair is ALPHA and is disabled by default. |
| settings.featureGates.spotToSpotConsolidation | bool | `false` | spotToSpotConsolidation is ALPHA and is disabled by default. |
| settings.featureGates.staticCapacity | bool | `false` | staticCapacity is ALPHA and is disabled by default. |
| settings.inventory | object | `{"enabled":false,"refreshPeriod":"5m","repairTolerationDuration":"30m"}` | Sled inventory configuration. Records which physical sled each node landed on, as labels and annotations on the Node and NodeClaim. This is observability only: nothing in the provider reads it, and provisioning is unaffected whether it is enabled, disabled, or failing. |
| settings.inventory.enabled | bool | `false` | Enable the sled inventory controllers. This requires an Oxide token with fleet-level read access to the system API (/v1/system/hardware/sleds), which is a much broader grant than provisioning instances into a single project. Leave this disabled unless the placement labels are wanted. If enabled with a token that cannot read the system API, the controller logs the failure and applies no labels rather than failing. |
| settings.inventory.refreshPeriod | string | `"5m"` | How often the sled inventory is re-read from the Oxide API. Building it costs one API call per sled in the rack, so lowering this raises fleet API load proportionally. |
| settings.inventory.repairTolerationDuration | string | `"30m"` | How long a node may sit on a sled that is being taken out of service (expunged, or closed to new placements) before Karpenter drains it and provisions a replacement. Replacement ALSO requires settings.featureGates.nodeRepair, and is subject to core's node repair guardrail that blocks repair while more than 20% of a NodePool's nodes are unhealthy. Set to 0 to record sled health without ever replacing nodes for it. |
| settings.minValuesPolicy | string | `"Strict"` | How the Karpenter scheduler treats min values. Options are 'Strict' and 'BestEffort'. |
| settings.oxide | object | `{"host":"","project":"","tokenSecret":{"key":"token","name":"oxide-api-token"}}` | Oxide API configuration. |
| settings.oxide.host | string | `""` | URL of the Oxide API, for example https://oxide.sys.example.com. |
| settings.oxide.project | string | `""` | Oxide project that Karpenter provisions instances into. |
| settings.oxide.tokenSecret | object | `{"key":"token","name":"oxide-api-token"}` | Secret in the release namespace holding the Oxide API token. The token is referenced rather than templated so it is never written into the Helm release. |
| settings.preferencePolicy | string | `"Respect"` | How the Karpenter scheduler should treat preferences. Can be one of 'Ignore' and 'Respect'. |
| strategy | object | `{"rollingUpdate":{"maxUnavailable":1}}` | Strategy for updating the pod. |
| terminationGracePeriodSeconds | string | `nil` | Override the default termination grace period for the pod. |
| tolerations | list | `[{"key":"CriticalAddonsOnly","operator":"Exists"}]` | Tolerations to allow the pod to be scheduled to nodes with taints. |
| topologySpreadConstraints | list | `[{"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"ScheduleAnyway"}]` | Topology spread constraints to increase the controller resilience by distributing pods across the cluster zones. If an explicit label selector is not provided one will be created from the pod selector labels. |

----------------------------------------------

Autogenerated from chart metadata using [helm-docs](https://github.com/norwoodj/helm-docs/).
