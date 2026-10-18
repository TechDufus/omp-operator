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

{{- define "omp-operator.authLabels" -}}
app.kubernetes.io/name: omp-auth
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "omp-operator.authSelectorLabels" -}}
app.kubernetes.io/name: omp-auth
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Image reference: repository@digest when a digest is set, else repository:tag (tag defaults to appVersion). */}}
{{- define "omp-operator.imageRef" -}}
{{- if .digest -}}
{{ .repository }}@{{ .digest }}
{{- else -}}
{{ .repository }}:{{ .tag | default .appVersion }}
{{- end -}}
{{- end -}}

{{- define "omp-operator.image" -}}
{{- include "omp-operator.imageRef" (dict "repository" .Values.image.repository "tag" .Values.image.tag "digest" .Values.image.digest "appVersion" .Chart.AppVersion) -}}
{{- end -}}

{{- define "omp-operator.runtimeImage" -}}
{{- include "omp-operator.imageRef" (dict "repository" .Values.runtimeImage.repository "tag" .Values.runtimeImage.tag "digest" .Values.runtimeImage.digest "appVersion" .Chart.AppVersion) -}}
{{- end -}}

{{/* Cluster-scoped object names carry the release name so two releases never collide. */}}
{{- define "omp-operator.managerRoleName" -}}
{{- printf "%s-omp-operator-manager" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "omp-operator.accessRoleName" -}}
{{- printf "%s-omp-operator-namespaced" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Namespaced rules R: what the operator needs in every namespace that holds agents. */}}
{{- define "omp-operator.namespacedRules" -}}
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch", "create", "delete"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "create", "update", "patch", "delete"]
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get"]
{{- end -}}
