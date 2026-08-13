{{- define "agent-sandbox.controllerArgs" -}}
{{- if hasKey .Values.controller "leaderElect" }}
- --leader-elect={{ .Values.controller.leaderElect }}
{{- end }}
{{- if hasKey .Values.controller "clusterDomain" }}
- --cluster-domain={{ .Values.controller.clusterDomain }}
{{- end }}
{{- if hasKey .Values.controller "leaderElectionNamespace" }}
- --leader-election-namespace={{ .Values.controller.leaderElectionNamespace }}
{{- end }}
{{- if hasKey .Values.controller "extensions" }}
- --extensions={{ .Values.controller.extensions }}
{{- end }}
{{- if hasKey .Values.controller "enableTracing" }}
- --enable-tracing={{ .Values.controller.enableTracing }}
{{- end }}
{{- if hasKey .Values.controller "enablePprof" }}
- --enable-pprof={{ .Values.controller.enablePprof }}
{{- end }}
{{- if hasKey .Values.controller "enablePprofDebug" }}
- --enable-pprof-debug={{ .Values.controller.enablePprofDebug }}
{{- end }}
{{- if hasKey .Values.controller "pprofBlockProfileRate" }}
- --pprof-block-profile-rate={{ .Values.controller.pprofBlockProfileRate }}
{{- end }}
{{- if hasKey .Values.controller "pprofMutexProfileFraction" }}
- --pprof-mutex-profile-fraction={{ .Values.controller.pprofMutexProfileFraction }}
{{- end }}
{{- if hasKey .Values.controller "kubeApiQps" }}
- --kube-api-qps={{ .Values.controller.kubeApiQps }}
{{- end }}
{{- if hasKey .Values.controller "kubeApiBurst" }}
- --kube-api-burst={{ .Values.controller.kubeApiBurst }}
{{- end }}
{{- if hasKey .Values.controller "sandboxConcurrentWorkers" }}
- --sandbox-concurrent-workers={{ .Values.controller.sandboxConcurrentWorkers }}
{{- end }}
{{- if hasKey .Values.controller "sandboxClaimConcurrentWorkers" }}
- --sandbox-claim-concurrent-workers={{ .Values.controller.sandboxClaimConcurrentWorkers }}
{{- end }}
{{- if hasKey .Values.controller "sandboxWarmPoolConcurrentWorkers" }}
- --sandbox-warm-pool-concurrent-workers={{ .Values.controller.sandboxWarmPoolConcurrentWorkers }}
{{- end }}
{{- if hasKey .Values.controller "sandboxTemplateConcurrentWorkers" }}
- --sandbox-template-concurrent-workers={{ .Values.controller.sandboxTemplateConcurrentWorkers }}
{{- end }}
{{- if hasKey .Values.controller "sandboxWarmPoolMaxBatchSize" }}
- --sandbox-warm-pool-max-batch-size={{ .Values.controller.sandboxWarmPoolMaxBatchSize }}
{{- end }}
{{- if hasKey .Values.controller "enableWarmPoolEviction" }}
- --enable-warm-pool-eviction={{ .Values.controller.enableWarmPoolEviction }}
{{- end }}
{{- if .Values.webhookServiceName }}
- --webhook-service-name={{ .Values.webhookServiceName }}
{{- end }}
{{- if (include "agent-sandbox.namespace" .) }}
- --webhook-namespace={{ include "agent-sandbox.namespace" . }}
{{- end }}
{{- range .Values.controller.extraArgs }}
- {{ . | quote }}
{{- end }}
{{- end }}
