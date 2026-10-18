{{- define "omp-operator.labels" -}}
app.kubernetes.io/name: omp-operator
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "omp-operator.selectorLabels" -}}
app.kubernetes.io/name: omp-operator
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
