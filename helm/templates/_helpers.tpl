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
Validate controller.watchNamespaces: every entry must be non-blank.
Include this template to fail early on lists like [""] or ["team-a", ""].
*/}}
{{- define "agent-sandbox.validateWatchNamespaces" -}}
{{- range .Values.controller.watchNamespaces }}
{{- if not (trim .) }}
{{- fail "controller.watchNamespaces contains a blank entry; every namespace must be a non-empty string" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The effective leader-election namespace: explicit if set, otherwise the
controller's deployment namespace.
*/}}
{{- define "agent-sandbox.leaderElectionNamespace" -}}
{{- default (include "agent-sandbox.namespace" .) .Values.controller.leaderElectionNamespace }}
{{- end }}

{{/*
The controller image reference.
*/}}
{{- define "agent-sandbox.image" -}}
{{- $tag := required "image.tag is required" .Values.image.tag }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}
