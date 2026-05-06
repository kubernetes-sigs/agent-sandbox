{{- define "agent-sandbox.controllerArgs" -}}
{{- if .Values.controller.leaderElect }}
- --leader-elect={{ .Values.controller.leaderElect }}
{{- end }}
{{- if .Values.controller.clusterDomain }}
- --cluster-domain={{ .Values.controller.clusterDomain }}
{{- end }}
{{- if .Values.controller.leaderElectionNamespace }}
- --leader-election-namespace={{ .Values.controller.leaderElectionNamespace }}
{{- end }}
{{- if .Values.controller.extensions }}
- --extensions={{ .Values.controller.extensions }}
{{- end }}
{{- if .Values.controller.enableTracing }}
- --enable-tracing={{ .Values.controller.enableTracing }}
{{- end }}
{{- if .Values.controller.enablePprof }}
- --enable-pprof={{ .Values.controller.enablePprof }}
{{- end }}
{{- if .Values.controller.enablePprofDebug }}
- --enable-pprof-debug={{ .Values.controller.enablePprofDebug }}
{{- end }}
{{- if .Values.controller.pprofBlockProfileRate }}
- --pprof-block-profile-rate={{ .Values.controller.pprofBlockProfileRate }}
{{- end }}
{{- if .Values.controller.pprofMutexProfileFraction }}
- --pprof-mutex-profile-fraction={{ .Values.controller.pprofMutexProfileFraction }}
{{- end }}
{{- if .Values.controller.kubeApiQps }}
- --kube-api-qps={{ .Values.controller.kubeApiQps }}
{{- end }}
{{- if .Values.controller.kubeApiBurst }}
- --kube-api-burst={{ .Values.controller.kubeApiBurst }}
{{- end }}
{{- if .Values.controller.sandboxConcurrentWorkers }}
- --sandbox-concurrent-workers={{ .Values.controller.sandboxConcurrentWorkers }}
{{- end }}
{{- if .Values.controller.sandboxClaimConcurrentWorkers }}
- --sandbox-claim-concurrent-workers={{ .Values.controller.sandboxClaimConcurrentWorkers }}
{{- end }}
{{- if .Values.controller.sandboxWarmPoolConcurrentWorkers }}
- --sandbox-warm-pool-concurrent-workers={{ .Values.controller.sandboxWarmPoolConcurrentWorkers }}
{{- end }}
{{- if .Values.controller.sandboxTemplateConcurrentWorkers }}
- --sandbox-template-concurrent-workers={{ .Values.controller.sandboxTemplateConcurrentWorkers }}
{{- end }}
{{- range .Values.controller.extraArgs }}
- {{ . | quote }}
{{- end }}
{{- end }}
