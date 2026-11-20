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
{{- default .Release.Namespace .Values.namespace.name }}
{{- end }}

{{/*
The controller Service name.
*/}}
{{- define "agent-sandbox.serviceName" -}}
{{- default "agent-sandbox-controller" .Values.service.name }}
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
{{- $tag := required "image.tag is required" .Values.image.tag }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
An integer flag value. Values files decode numbers as float64, so a whole
float in the int64 range renders as an integer instead of 1e+06. Anything
else renders as given so the binary still rejects invalid input.
*/}}
{{- define "agent-sandbox.int" -}}
{{- if and (kindIs "float64" .) (eq . (floor .)) (ge . -9.223372036854775808e18) (lt . 9.223372036854775808e18) }}{{ int64 . }}{{ else }}{{ . }}{{ end }}
{{- end }}
