# karpenter-provider-oxide

![Version: 0.0.1](https://img.shields.io/badge/Version-0.0.1-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.1](https://img.shields.io/badge/AppVersion-0.0.1-informational?style=flat-square)

A Helm chart for the Karpenter Oxide provider controller.

**Homepage:** <https://github.com/oxidecomputer/karpenter-provider-oxide>

## Installing the Chart

To install the chart with the release name `karpenter`:

```console
helm upgrade --install karpenter oci://ghcr.io/oxidecomputer/helm-charts/karpenter-provider-oxide \
  --namespace kube-system \
  --set settings.oxide.host=${OXIDE_HOST} \
  --set settings.oxide.project=${OXIDE_PROJECT}
```

The controller reads its Oxide API token from a Secret in the release namespace
(`settings.oxide.tokenSecret`), which is not created by this chart:

```console
kubectl create secret --namespace kube-system generic oxide-api-token --from-literal=token=${OXIDE_TOKEN}
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
| bootstrap | object | `{"kubeadm":{"enabled":false},"rke2":{"enabled":true,"tokenSecret":{"name":"rancher-agent-token","namespace":""}}}` | Bootstrap provider configuration. |
| bootstrap.kubeadm.enabled | bool | `false` | Enable RBAC for the kubeadm bootstrap provider. |
| bootstrap.rke2.enabled | bool | `true` | Enable RBAC for the RKE2 bootstrap provider. |
| bootstrap.rke2.tokenSecret | object | `{"name":"rancher-agent-token","namespace":""}` | The Secret holding the RKE2 agent join token. |
| bootstrap.rke2.tokenSecret.name | string | `"rancher-agent-token"` | Name of the token Secret. |
| bootstrap.rke2.tokenSecret.namespace | string | `""` | Namespace of the token Secret. |
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
| serviceAccount.name | string | `""` | The name of the ServiceAccount to use. |
| serviceMonitor.additionalLabels | object | `{}` | Additional labels for the ServiceMonitor. |
| serviceMonitor.enabled | bool | `false` | Specifies whether a ServiceMonitor should be created. |
| serviceMonitor.endpointConfig | object | `{}` | Endpoint configuration for the ServiceMonitor. |
| settings | object | `{"batchIdleDuration":"1s","batchMaxDuration":"10s","featureGates":{"nodeOverlay":false,"nodeRepair":false,"spotToSpotConsolidation":false,"staticCapacity":false},"instanceTypes":{"incrementPercent":10},"inventory":{"enabled":false,"refreshPeriod":"5m","repairTolerationDuration":"30m"},"launch":{"deadline":"10m","diskCreatingDeadline":"2m","instanceCreatingDeadline":"2m","instanceStartingDeadline":"5m","instanceStoppingDeadline":"3m"},"minValuesPolicy":"Strict","oxide":{"clusterID":"","host":"","project":"","tokenSecret":{"key":"token","name":"oxide-api-token"}},"preferencePolicy":"Respect","quota":{"enabled":true,"refreshPeriod":"1m"},"reaper":{"enabled":true,"period":"1m","quarantine":"5m"}}` | Global settings to configure Karpenter. |
| settings.batchIdleDuration | string | `"1s"` | The maximum amount of time with no new pending pods that if exceeded ends the current batching window. |
| settings.batchMaxDuration | string | `"10s"` | The maximum length of a batch window. |
| settings.featureGates | object | `{"nodeOverlay":false,"nodeRepair":false,"spotToSpotConsolidation":false,"staticCapacity":false}` | Feature gate configuration values. |
| settings.featureGates.nodeOverlay | bool | `false` | nodeOverlay is ALPHA and is disabled by default. |
| settings.featureGates.nodeRepair | bool | `false` | nodeRepair is ALPHA and is disabled by default. |
| settings.featureGates.spotToSpotConsolidation | bool | `false` | spotToSpotConsolidation is ALPHA and is disabled by default. |
| settings.featureGates.staticCapacity | bool | `false` | staticCapacity is ALPHA and is disabled by default. |
| settings.instanceTypes | object | `{"incrementPercent":10}` | Instance type catalog generation. |
| settings.instanceTypes.incrementPercent | int | `10` | Percentage increase between successive CPU and memory values in the generated catalog. |
| settings.inventory | object | `{"enabled":false,"refreshPeriod":"5m","repairTolerationDuration":"30m"}` | Sled inventory configuration. |
| settings.inventory.enabled | bool | `false` | Enable the sled inventory controllers. |
| settings.inventory.refreshPeriod | string | `"5m"` | How often the sled inventory is re-read from the Oxide API. |
| settings.inventory.repairTolerationDuration | string | `"30m"` | Time before replacing a node on an unhealthy sled. |
| settings.launch | object | `{"deadline":"10m","diskCreatingDeadline":"2m","instanceCreatingDeadline":"2m","instanceStartingDeadline":"5m","instanceStoppingDeadline":"3m"}` | Launch deadlines. |
| settings.launch.deadline | string | `"10m"` | Overall bound on a single launch, across every Oxide API call it makes. |
| settings.launch.diskCreatingDeadline | string | `"2m"` | How long a disk may sit in the creating state. |
| settings.launch.instanceCreatingDeadline | string | `"2m"` | How long an instance may sit in the creating state. |
| settings.launch.instanceStartingDeadline | string | `"5m"` | Node registration deadline for a placed instance. |
| settings.launch.instanceStoppingDeadline | string | `"3m"` | How long a teardown waits for an instance to reach a state the Oxide API accepts a delete in. |
| settings.minValuesPolicy | string | `"Strict"` | How the Karpenter scheduler treats min values. |
| settings.oxide | object | `{"clusterID":"","host":"","project":"","tokenSecret":{"key":"token","name":"oxide-api-token"}}` | Oxide API configuration. |
| settings.oxide.clusterID | string | `""` | Identity this cluster records on every Oxide resource it creates. |
| settings.oxide.host | string | `""` | URL of the Oxide API, for example https://oxide.sys.example.com. |
| settings.oxide.project | string | `""` | Oxide project that Karpenter provisions instances into. |
| settings.oxide.tokenSecret | object | `{"key":"token","name":"oxide-api-token"}` | Secret in the release namespace holding the Oxide API token. |
| settings.preferencePolicy | string | `"Respect"` | How the Karpenter scheduler should treat preferences. |
| settings.quota | object | `{"enabled":true,"refreshPeriod":"1m"}` | Silo quota configuration. |
| settings.quota.enabled | bool | `true` | Enable silo quota awareness. |
| settings.quota.refreshPeriod | string | `"1m"` | How often the silo's quota is re-read. |
| settings.reaper | object | `{"enabled":true,"period":"1m","quarantine":"5m"}` | Orphan reaper. |
| settings.reaper.enabled | bool | `true` | Enable the orphan reaper. |
| settings.reaper.period | string | `"1m"` | How often the project is rescanned. |
| settings.reaper.quarantine | string | `"5m"` | How long a resource must look orphaned before it is deleted. |
| strategy | object | `{"rollingUpdate":{"maxUnavailable":1}}` | Strategy for updating the pod. |
| terminationGracePeriodSeconds | string | `nil` | Override the default termination grace period for the pod. |
| tolerations | list | `[{"key":"CriticalAddonsOnly","operator":"Exists"}]` | Tolerations to allow the pod to be scheduled to nodes with taints. |
| topologySpreadConstraints | list | `[{"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"ScheduleAnyway"}]` | Topology spread constraints to increase the controller resilience by distributing pods across the cluster zones. |

----------------------------------------------

Autogenerated from chart metadata using [helm-docs](https://github.com/norwoodj/helm-docs/).
