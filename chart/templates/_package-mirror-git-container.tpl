{{/*
Shared git-cache container spec. Used both inline in the main package-mirror pod
(when packageMirror.git.standalone is false) and as the sole container of the
standalone git Deployment. Authored at zero indentation; callers must render it
with `nindent 8` under a `containers:` list.
*/}}
{{- define "buildkit-service.packageMirror.gitContainer" -}}
- name: git
  image: {{ printf "%s:%s" .Values.packageMirror.git.image.repository .Values.packageMirror.git.image.tag }}
  imagePullPolicy: {{ .Values.packageMirror.git.image.pullPolicy }}
  securityContext:
    {{- toYaml .Values.packageMirror.securityContext | nindent 4 }}
  env:
    - name: GIT_CACHE_ROOT
      value: /var/cache/git-mirror
    - name: GIT_CACHE_PORT
      value: {{ .Values.packageMirror.git.port | quote }}
    - name: GIT_CACHE_TTL_SECONDS
      value: {{ .Values.packageMirror.git.env.cacheTtlSeconds | quote }}
    - name: GIT_CACHE_MAX_ACTIVE_REQUESTS
      value: {{ .Values.packageMirror.git.env.maxActiveRequests | quote }}
    - name: GIT_CACHE_BUSY_TIMEOUT_SECONDS
      value: {{ .Values.packageMirror.git.env.busyTimeoutSeconds | quote }}
    - name: GIT_CACHE_MAX_REQUEST_BODY_BYTES
      value: {{ .Values.packageMirror.git.env.maxRequestBodyBytes | int64 | quote }}
    - name: GIT_CACHE_REQUEST_READ_TIMEOUT_SECONDS
      value: {{ .Values.packageMirror.git.env.requestReadTimeoutSeconds | int | quote }}
    - name: GIT_CACHE_ALLOW_PRIVATE_UPSTREAMS
      value: {{ .Values.packageMirror.git.env.allowPrivateUpstreams | quote }}
    - name: GIT_CACHE_UPSTREAM_SCHEME
      value: {{ .Values.packageMirror.git.env.upstreamScheme | quote }}
    {{- if .Values.packageMirror.git.env.gitTimeoutSeconds }}
    - name: GIT_CACHE_GIT_TIMEOUT_SECONDS
      value: {{ .Values.packageMirror.git.env.gitTimeoutSeconds | quote }}
    {{- end }}
    {{- if .Values.packageMirror.git.env.maxConcurrentClones }}
    - name: GIT_CACHE_MAX_CONCURRENT_CLONES
      value: {{ .Values.packageMirror.git.env.maxConcurrentClones | quote }}
    {{- end }}
    {{- if .Values.packageMirror.git.env.packThreads }}
    - name: GIT_CACHE_PACK_THREADS
      value: {{ .Values.packageMirror.git.env.packThreads | quote }}
    {{- end }}
    {{- if .Values.packageMirror.git.env.packWindowMemory }}
    - name: GIT_CACHE_PACK_WINDOW_MEMORY
      value: {{ .Values.packageMirror.git.env.packWindowMemory | quote }}
    {{- end }}
    {{- if .Values.packageMirror.git.env.packDeltaCacheSize }}
    - name: GIT_CACHE_PACK_DELTA_CACHE_SIZE
      value: {{ .Values.packageMirror.git.env.packDeltaCacheSize | quote }}
    {{- end }}
    {{- if .Values.packageMirror.git.env.maxDiskBytes }}
    - name: GIT_CACHE_MAX_DISK_BYTES
      value: {{ .Values.packageMirror.git.env.maxDiskBytes | quote }}
    - name: GIT_CACHE_GC_INTERVAL_SECONDS
      value: {{ .Values.packageMirror.git.env.gcIntervalSeconds | quote }}
    - name: GIT_CACHE_GC_TARGET_RATIO
      value: {{ .Values.packageMirror.git.env.gcTargetRatio | quote }}
    {{- end }}
    {{- if .Values.packageMirror.git.env.packThreads }}
    # Bound pack-objects memory on the CURRENT image too: git-cache-server
    # copies the container env into git-http-backend, which forwards these
    # GIT_CONFIG_* settings to git-upload-pack / pack-objects.
    - name: GIT_CONFIG_COUNT
      value: "3"
    - name: GIT_CONFIG_KEY_0
      value: pack.threads
    - name: GIT_CONFIG_VALUE_0
      value: {{ .Values.packageMirror.git.env.packThreads | quote }}
    - name: GIT_CONFIG_KEY_1
      value: pack.windowMemory
    - name: GIT_CONFIG_VALUE_1
      value: {{ .Values.packageMirror.git.env.packWindowMemory | quote }}
    - name: GIT_CONFIG_KEY_2
      value: pack.deltaCacheSize
    - name: GIT_CONFIG_VALUE_2
      value: {{ .Values.packageMirror.git.env.packDeltaCacheSize | quote }}
    {{- end }}
  ports:
    - name: {{ .Values.packageMirror.git.portName }}
      containerPort: {{ .Values.packageMirror.git.port }}
      protocol: {{ .Values.packageMirror.git.protocol }}
  livenessProbe:
    httpGet:
      path: {{ .Values.packageMirror.git.livenessProbe.path }}
      port: {{ .Values.packageMirror.git.portName }}
    initialDelaySeconds: {{ .Values.packageMirror.git.livenessProbe.initialDelaySeconds }}
    periodSeconds: {{ .Values.packageMirror.git.livenessProbe.periodSeconds }}
    timeoutSeconds: {{ .Values.packageMirror.git.livenessProbe.timeoutSeconds }}
    failureThreshold: {{ .Values.packageMirror.git.livenessProbe.failureThreshold }}
    successThreshold: {{ .Values.packageMirror.git.livenessProbe.successThreshold }}
  readinessProbe:
    httpGet:
      path: {{ .Values.packageMirror.git.readinessProbe.path }}
      port: {{ .Values.packageMirror.git.portName }}
    initialDelaySeconds: {{ .Values.packageMirror.git.readinessProbe.initialDelaySeconds }}
    periodSeconds: {{ .Values.packageMirror.git.readinessProbe.periodSeconds }}
    timeoutSeconds: {{ .Values.packageMirror.git.readinessProbe.timeoutSeconds }}
    failureThreshold: {{ .Values.packageMirror.git.readinessProbe.failureThreshold }}
    successThreshold: {{ .Values.packageMirror.git.readinessProbe.successThreshold }}
  resources:
    {{- toYaml (default .Values.packageMirror.resources .Values.packageMirror.git.resources) | nindent 4 }}
  volumeMounts:
    {{- if .Values.packageMirror.git.persistence.enabled }}
    - name: {{ include "buildkit-service.packageMirror.gitCacheVolumeName" . }}
      mountPath: /var/cache/git-mirror
    {{- else if and (not .Values.packageMirror.git.standalone) .Values.packageMirror.persistence.enabled }}
    - name: {{ include "buildkit-service.packageMirror.cacheVolumeName" . }}
      mountPath: /var/cache/git-mirror
      subPath: git-mirror
    {{- else }}
    - name: {{ include "buildkit-service.packageMirror.gitCacheVolumeName" . }}
      mountPath: /var/cache/git-mirror
    {{- end }}
{{- end -}}
