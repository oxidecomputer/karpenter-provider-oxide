{{- define "oxide-kwok.name" -}}
{{- printf "%s-kwok" .Release.Name | trunc 48 | trimSuffix "-" -}}
{{- end -}}
{{- define "oxide-kwok.labels" -}}
app.kubernetes.io/part-of: oxide-kwok
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}
