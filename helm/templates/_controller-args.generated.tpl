{{- define "agent-sandbox.controllerArgs" -}}
- --leader-elect={{ .Values.controller.leaderElect }}
- --cluster-domain={{ .Values.controller.clusterDomain }}
{{- if .Values.controller.leaderElectionNamespace }}
- --leader-election-namespace={{ .Values.controller.leaderElectionNamespace }}
{{- end }}
- --extensions={{ .Values.controller.extensions }}
- --enable-tracing={{ .Values.controller.enableTracing }}
- --enable-pprof={{ .Values.controller.enablePprof }}
- --enable-pprof-debug={{ .Values.controller.enablePprofDebug }}
- --pprof-block-profile-rate={{ .Values.controller.pprofBlockProfileRate }}
- --pprof-mutex-profile-fraction={{ .Values.controller.pprofMutexProfileFraction }}
- --kube-api-qps={{ .Values.controller.kubeApiQps }}
- --kube-api-burst={{ .Values.controller.kubeApiBurst }}
- --sandbox-concurrent-workers={{ .Values.controller.sandboxConcurrentWorkers }}
- --sandbox-claim-concurrent-workers={{ .Values.controller.sandboxClaimConcurrentWorkers }}
- --sandbox-warm-pool-concurrent-workers={{ .Values.controller.sandboxWarmPoolConcurrentWorkers }}
- --sandbox-template-concurrent-workers={{ .Values.controller.sandboxTemplateConcurrentWorkers }}
{{- range .Values.controller.extraArgs }}
- {{ . | quote }}
{{- end }}
{{- end }}
