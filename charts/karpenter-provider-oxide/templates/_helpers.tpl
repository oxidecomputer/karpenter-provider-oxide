{{- define "karpenter.name" -}}
{{- default "karpenter" .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "karpenter.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default "karpenter" .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "karpenter.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "karpenter.labels" -}}
helm.sh/chart: {{ include "karpenter.chart" . }}
{{ include "karpenter.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.additionalLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "karpenter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "karpenter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "karpenter.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "karpenter.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "karpenter.controller.image" -}}
{{- if .Values.controller.image.digest }}
{{- printf "%s:%s@%s" .Values.controller.image.repository (default .Chart.AppVersion .Values.controller.image.tag) .Values.controller.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.controller.image.repository (default .Chart.AppVersion .Values.controller.image.tag) }}
{{- end }}
{{- end }}


{{- define "karpenter.pdb.apiVersion" -}}
{{- if and (.Capabilities.APIVersions.Has "policy/v1") (semverCompare ">= 1.21-0" .Capabilities.KubeVersion.Version) -}}
{{- print "policy/v1" -}}
{{- else -}}
{{- print "policy/v1beta1" -}}
{{- end -}}
{{- end -}}

{{- define "karpenter.patchLabelSelector" -}}
{{- if not (hasKey ._target "labelSelector") }}
{{- $selectorLabels := (include "karpenter.selectorLabels" .) | fromYaml }}
{{- $_ := set ._target "labelSelector" (dict "matchLabels" $selectorLabels) }}
{{- end }}
{{- end }}

{{- define "karpenter.patchPodAffinity" -}}
{{- if (hasKey ._podAffinity "requiredDuringSchedulingIgnoredDuringExecution") }}
{{- range $term := ._podAffinity.requiredDuringSchedulingIgnoredDuringExecution }}
{{- include "karpenter.patchLabelSelector" (merge (dict "_target" $term) $) }}
{{- end }}
{{- end }}
{{- if (hasKey ._podAffinity "preferredDuringSchedulingIgnoredDuringExecution") }}
{{- range $weightedTerm := ._podAffinity.preferredDuringSchedulingIgnoredDuringExecution }}
{{- include "karpenter.patchLabelSelector" (merge (dict "_target" $weightedTerm.podAffinityTerm) $) }}
{{- end }}
{{- end }}
{{- end }}

{{- define "karpenter.patchAffinity" -}}
{{- if (hasKey .Values.affinity "podAffinity") }}
{{- include "karpenter.patchPodAffinity" (merge (dict "_podAffinity" .Values.affinity.podAffinity) .) }}
{{- end }}
{{- if (hasKey .Values.affinity "podAntiAffinity") }}
{{- include "karpenter.patchPodAffinity" (merge (dict "_podAffinity" .Values.affinity.podAntiAffinity) .) }}
{{- end }}
{{- end }}

{{- define "karpenter.patchTopologySpreadConstraints" -}}
{{- range $constraint := .Values.topologySpreadConstraints }}
{{- include "karpenter.patchLabelSelector" (merge (dict "_target" $constraint) $) }}
{{- end }}
{{- end }}

{{- define "karpenter.outputPathsList" -}}
{{ $paths := list -}}
{{- range .Values.logConfig.outputPaths -}}
    {{- if not (has (printf "%s" . | quote) $paths) -}}
        {{- $paths = printf "%s" . | quote  | append $paths -}}
    {{- end -}}
{{- end -}}
{{ $paths | join ", " }}
{{- end -}}

{{- define "karpenter.errorOutputPathsList" -}}
{{ $paths := list -}}
{{- range .Values.logConfig.errorOutputPaths -}}
    {{- if not (has (printf "%s" . | quote) $paths) -}}
        {{- $paths = printf "%s" . | quote  | append $paths -}}
    {{- end -}}
{{- end -}}
{{ $paths | join ", " }}
{{- end -}}
