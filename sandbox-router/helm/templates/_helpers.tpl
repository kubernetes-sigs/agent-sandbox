{{/*
The component name. Resource names and selector labels are fixed to match the
kustomize base in deploy/, so the Service and PDB keep selecting the Deployment.
*/}}
{{- define "sandbox-router.name" -}}
sandbox-router
{{- end }}

{{/*
The namespace to deploy into.
*/}}
{{- define "sandbox-router.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name }}
{{- end }}

{{/*
The router Service name.
*/}}
{{- define "sandbox-router.serviceName" -}}
{{- default "sandbox-router-svc" .Values.service.name }}
{{- end }}

{{/*
Selector labels used by the Deployment, Service, PDB and NetworkPolicy. They
match the labels of the kustomize base in deploy/.
*/}}
{{- define "sandbox-router.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sandbox-router.name" . }}
app.kubernetes.io/component: sandbox-router
{{- end }}

{{/*
Common labels.
*/}}
{{- define "sandbox-router.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{ include "sandbox-router.selectorLabels" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Values.image.tag }}
app.kubernetes.io/version: {{ .Values.image.tag | quote }}
{{- end }}
{{- end }}

{{/*
The router image reference.
*/}}
{{- define "sandbox-router.image" -}}
{{- $tag := required "image.tag is required" .Values.image.tag }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
The authorization mode, rejecting values the router does not know.
*/}}
{{- define "sandbox-router.authzMode" -}}
{{- if not (has .Values.authz.mode (list "allow-all" "tokenreview" "scoped-token")) }}
{{- fail (printf "authz.mode must be allow-all, tokenreview or scoped-token, got %q" .Values.authz.mode) }}
{{- end }}
{{- .Values.authz.mode }}
{{- end }}

{{/*
Router command-line arguments. Unset optional flags are omitted.
*/}}
{{- define "sandbox-router.args" -}}
{{- if and .Values.tls.metricsSecureServing (not .Values.tls.enabled) }}
{{- fail "tls.metricsSecureServing requires tls.enabled=true" }}
{{- end }}
- --http-bind-address=:8080
{{- if .Values.tls.enabled }}
- --https-bind-address=:8443
- --tls-cert-file=/tls/tls.crt
- --tls-key-file=/tls/tls.key
{{- if .Values.tls.metricsSecureServing }}
- --metrics-secure-serving
{{- end }}
{{- end }}
- --metrics-bind-address=:9090
- --health-probe-bind-address=:8081
- --cluster-domain={{ .Values.router.clusterDomain }}
- --proxy-timeout={{ .Values.router.proxyTimeout }}
- --upstream-max-retries={{ .Values.router.upstreamMaxRetries }}
- --cache-enabled={{ .Values.router.cacheEnabled }}
{{- with .Values.router.cacheNamespace }}
- --cache-namespace={{ . }}
{{- end }}
- --authz-mode={{ include "sandbox-router.authzMode" . }}
{{- if not (kindIs "invalid" .Values.router.enableTracing) }}
- --enable-tracing={{ .Values.router.enableTracing }}
{{- end }}
{{- if not (kindIs "invalid" .Values.router.enableOtelMetrics) }}
- --enable-otel-metrics={{ .Values.router.enableOtelMetrics }}
{{- end }}
{{- if not (kindIs "invalid" .Values.router.accessLog) }}
- --access-log={{ .Values.router.accessLog }}
{{- end }}
{{- if not (kindIs "invalid" .Values.router.maxRequestBodyBytes) }}
- --max-request-body-bytes={{ .Values.router.maxRequestBodyBytes }}
{{- end }}
{{- with .Values.router.pathRoutingPrefix }}
- --path-routing-prefix={{ . }}
{{- end }}
{{- range .Values.router.extraArgs }}
- {{ . | quote }}
{{- end }}
{{- end }}
