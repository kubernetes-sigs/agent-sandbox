{{/*
Expand the name of the chart.
*/}}
{{- define "agent-sandbox.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The namespace to deploy into.
*/}}
{{- define "agent-sandbox.namespace" -}}
{{- .Values.namespace.name }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "agent-sandbox.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/name: {{ include "agent-sandbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Values.image.tag }}
app.kubernetes.io/version: {{ .Values.image.tag | quote }}
{{- end }}
{{- end }}

{{/*
Selector labels used by Deployment and Service.
*/}}
{{- define "agent-sandbox.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agent-sandbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The controller image reference.
*/}}
{{- define "agent-sandbox.image" -}}
{{- $tag := .Values.image.tag }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}
