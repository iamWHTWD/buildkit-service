{{- define "buildkit-service.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "buildkit-service.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "buildkit-service.name" . -}}
{{- end -}}
{{- end -}}

{{- define "buildkit-service.buildctlDaemonName" -}}
{{- printf "%s-buildctl-daemon" (include "buildkit-service.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "buildkit-service.buildctlDaemonSecretName" -}}
{{- printf "%s-token" (include "buildkit-service.buildctlDaemonName" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "buildkit-service.buildctlDaemonCanaryName" -}}
{{- printf "%s-buildctl-daemon-canary" (include "buildkit-service.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "buildkit-service.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end -}}

{{- define "buildkit-service.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" -}}
{{- end -}}

{{- define "buildkit-service.selectorLabels" -}}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/name: {{ include "buildkit-service.name" . }}
{{- end -}}

{{- define "buildkit-service.labels" -}}
helm.sh/chart: {{ include "buildkit-service.chart" . }}
app.kubernetes.io/name: {{ include "buildkit-service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "buildkit-service.registryConfigJson" -}}
{{- $registries := .Values.registries | default list -}}
{{- if eq (len $registries) 0 -}}
{{- fail "Values.registries must contain at least one registry entry" -}}
{{- end -}}
{{- $auths := dict -}}
{{- range $index, $entry := $registries -}}
{{- $host := $entry.authHost | default $entry.host -}}
{{- if not $host -}}
{{- fail (printf "Values.registries[%d].host or authHost is required" $index) -}}
{{- end -}}
{{- with $entry.auth -}}
{{- $_ := set $auths $host (dict "auth" .) -}}
{{- end -}}
{{- end -}}
{{- dict "auths" $auths | toJson -}}
{{- end -}}

{{- define "buildkit-service.registryConfigJsonB64" -}}
{{- include "buildkit-service.registryConfigJson" . | b64enc -}}
{{- end -}}

{{- define "buildkit-service.registryHostRules" -}}
{{- $root := .root -}}
{{- $daemon := .daemon | default dict -}}
{{- $rules := list -}}
{{- $entries := dict -}}
{{- $order := list -}}
{{- range $root.Values.registries | default list -}}
{{- $host := trimSuffix "." (lower (trim .host)) -}}
{{- if $host -}}
{{- $entry := get $entries $host | default (dict "targets" (list) "seen" (dict)) -}}
{{- if not (hasKey $entries $host) -}}
{{- $_ := set $entries $host $entry -}}
{{- $order = append $order $host -}}
{{- end -}}
{{- $targets := get $entry "targets" -}}
{{- $seen := get $entry "seen" -}}
{{- if not (hasKey $seen $host) -}}
{{- $_ := set $seen $host true -}}
{{- $targets = append $targets $host -}}
{{- end -}}
{{- $remoteWithoutScheme := trimPrefix "http://" (trimPrefix "https://" (.remoteUrl | default "")) -}}
{{- $remoteHost := trimSuffix "." (lower (trim (first (splitList "/" $remoteWithoutScheme)))) -}}
{{- if and $remoteHost (not (hasKey $seen $remoteHost)) -}}
{{- $_ := set $seen $remoteHost true -}}
{{- $targets = append $targets $remoteHost -}}
{{- end -}}
{{- range .tokenHosts | default list -}}
{{- $tokenHost := trimSuffix "." (lower (trim .)) -}}
{{- if and $tokenHost (not (hasKey $seen $tokenHost)) -}}
{{- $_ := set $seen $tokenHost true -}}
{{- $targets = append $targets $tokenHost -}}
{{- end -}}
{{- end -}}
{{- $_ := set $entry "targets" $targets -}}
{{- end -}}
{{- end -}}
{{- range $daemon.routingTargets | default list -}}
{{- $prefixWithoutScheme := trimPrefix "http://" (trimPrefix "https://" (trim .)) -}}
{{- $host := trimSuffix "." (lower (trim (first (splitList "/" $prefixWithoutScheme)))) -}}
{{- if $host -}}
{{- $entry := get $entries $host | default (dict "targets" (list) "seen" (dict)) -}}
{{- if not (hasKey $entries $host) -}}
{{- $_ := set $entries $host $entry -}}
{{- $order = append $order $host -}}
{{- end -}}
{{- $targets := get $entry "targets" -}}
{{- $seen := get $entry "seen" -}}
{{- if not (hasKey $seen $host) -}}
{{- $_ := set $seen $host true -}}
{{- $targets = append $targets $host -}}
{{- end -}}
{{- range $daemon.registryTokenHosts | default list -}}
{{- $tokenHost := trimSuffix "." (lower (trim .)) -}}
{{- if and $tokenHost (not (hasKey $seen $tokenHost)) -}}
{{- $_ := set $seen $tokenHost true -}}
{{- $targets = append $targets $tokenHost -}}
{{- end -}}
{{- end -}}
{{- $_ := set $entry "targets" $targets -}}
{{- end -}}
{{- end -}}
{{- range $order -}}
{{- $entry := get $entries . -}}
{{- $rules = append $rules (printf "%s=%s" . (join "|" (get $entry "targets"))) -}}
{{- end -}}
{{- join ";" $rules -}}
{{- end -}}

{{- define "buildkit-service.image" -}}
{{- if kindIs "string" .Values.image -}}
{{- required "Values.image is required" .Values.image -}}
{{- else -}}
{{- $imageRepository := required "Values.image.repository is required" .Values.image.repository -}}
{{- $imageTag := required "Values.image.tag is required" .Values.image.tag -}}
{{- printf "%s:%s" $imageRepository $imageTag -}}
{{- end -}}
{{- end -}}

{{- define "buildkit-service.imageHost" -}}
{{- $image := include "buildkit-service.image" . -}}
{{- $first := first (splitList "/" $image) -}}
{{- if or (contains "." $first) (contains ":" $first) (eq $first "localhost") -}}
{{- $first -}}
{{- else -}}
docker.io
{{- end -}}
{{- end -}}

{{- define "buildkit-service.imagePullPolicy" -}}
{{- if kindIs "string" .Values.image -}}
{{- default "IfNotPresent" .Values.imagePullPolicy -}}
{{- else -}}
{{- default "IfNotPresent" .Values.image.pullPolicy -}}
{{- end -}}
{{- end -}}

{{- define "buildkit-service.buildctlDaemonImage" -}}
{{- if kindIs "string" .Values.buildctlDaemon.image -}}
{{- required "Values.buildctlDaemon.image is required" .Values.buildctlDaemon.image -}}
{{- else -}}
{{- $imageRepository := required "Values.buildctlDaemon.image.repository is required" .Values.buildctlDaemon.image.repository -}}
{{- $imageTag := required "Values.buildctlDaemon.image.tag is required" .Values.buildctlDaemon.image.tag -}}
{{- printf "%s:%s" $imageRepository $imageTag -}}
{{- end -}}
{{- end -}}

{{- define "buildkit-service.buildctlDaemonImagePullPolicy" -}}
{{- if kindIs "string" .Values.buildctlDaemon.image -}}
{{- default "IfNotPresent" .Values.buildctlDaemon.imagePullPolicy -}}
{{- else -}}
{{- default "IfNotPresent" .Values.buildctlDaemon.image.pullPolicy -}}
{{- end -}}
{{- end -}}

{{- define "buildkit-service.imagePullSecretName" -}}
{{- if kindIs "map" .Values.imagePullSecrets -}}
{{- .Values.imagePullSecrets.name | default "" -}}
{{- end -}}
{{- end -}}

{{- define "buildkit-service.imagePullSecretsEnabled" -}}
{{- if kindIs "map" .Values.imagePullSecrets -}}
{{- ternary "true" "false" (.Values.imagePullSecrets.create | default false) -}}
{{- else -}}
{{- ternary "true" "false" (gt (len (.Values.imagePullSecrets | default list)) 0) -}}
{{- end -}}
{{- end -}}

{{- define "buildkit-service.renderImagePullSecrets" -}}
{{- if kindIs "slice" .Values.imagePullSecrets -}}
{{- toYaml .Values.imagePullSecrets -}}
{{- else if kindIs "map" .Values.imagePullSecrets -}}
{{- $secretName := include "buildkit-service.imagePullSecretName" . -}}
{{- if $secretName -}}
- name: {{ $secretName }}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "buildkit-service.packageMirror.name" -}}
{{- default "package-mirror" .Values.packageMirror.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "buildkit-service.packageMirror.fullname" -}}
{{- if .Values.packageMirror.fullnameOverride -}}
{{- .Values.packageMirror.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "buildkit-service.packageMirror.name" . -}}
{{- end -}}
{{- end -}}

{{- define "buildkit-service.packageMirror.namespace" -}}
{{- default .Release.Namespace .Values.packageMirror.namespaceOverride -}}
{{- end -}}

{{- define "buildkit-service.packageMirror.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" -}}
{{- end -}}

{{- define "buildkit-service.packageMirror.selectorLabels" -}}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/name: {{ include "buildkit-service.packageMirror.name" . }}
{{- end -}}

{{- define "buildkit-service.packageMirror.registrySelectorLabels" -}}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/name: {{ include "buildkit-service.packageMirror.name" . }}-registry
{{- end -}}

{{- define "buildkit-service.packageMirror.gitFullname" -}}
{{- printf "%s-git" (include "buildkit-service.packageMirror.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "buildkit-service.packageMirror.gitSelectorLabels" -}}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/name: {{ include "buildkit-service.packageMirror.name" . }}-git
{{- end -}}

{{- define "buildkit-service.packageMirror.labels" -}}
helm.sh/chart: {{ include "buildkit-service.packageMirror.chart" . }}
app.kubernetes.io/name: {{ include "buildkit-service.packageMirror.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "buildkit-service.packageMirror.pip.extraIndexUrls" -}}
{{- join "," (.Values.packageMirror.pip.env.extraIndexUrls | default list) -}}
{{- end -}}

{{- define "buildkit-service.packageMirror.pip.extraIndexTtls" -}}
{{- $ttls := list -}}
{{- range .Values.packageMirror.pip.env.extraIndexTtls | default list -}}
{{- $ttls = append $ttls (toString .) -}}
{{- end -}}
{{- join "," $ttls -}}
{{- end -}}

{{- define "buildkit-service.packageMirror.cacheVolumeName" -}}
cache
{{- end -}}

{{- define "buildkit-service.packageMirror.pipCacheVolumeName" -}}
cache-pip
{{- end -}}

{{- define "buildkit-service.packageMirror.npmCacheVolumeName" -}}
cache-npm
{{- end -}}

{{- define "buildkit-service.packageMirror.aptYumCacheVolumeName" -}}
cache-apt-yum
{{- end -}}

{{- define "buildkit-service.packageMirror.gitCacheVolumeName" -}}
cache-git
{{- end -}}

{{- define "buildkit-service.packageMirror.registryCacheVolumeName" -}}
cache-registry
{{- end -}}

{{- define "buildkit-service.packageMirror.configVolumeName" -}}
config
{{- end -}}

{{- define "buildkit-service.packageMirror.selectedRegistryMirrors" -}}
{{- $imageHost := include "buildkit-service.imageHost" . -}}
{{- $selected := list -}}
{{- range $index, $entry := .Values.registries | default list -}}
{{- $mirror := $entry.mirror | default dict -}}
{{- $mirrorEnabled := true -}}
{{- if hasKey $mirror "enabled" -}}
{{- $mirrorEnabled = $mirror.enabled -}}
{{- end -}}
{{- if $mirrorEnabled -}}
{{- $remoteWithoutScheme := trimPrefix "http://" (trimPrefix "https://" (.remoteUrl | default "")) -}}
{{- $remoteHost := first (splitList "/" $remoteWithoutScheme) -}}
{{- if or ($mirror.always | default false) (eq $remoteHost $imageHost) -}}
{{- if not $entry.name -}}
{{- fail (printf "Values.registries[%d].name is required" $index) -}}
{{- end -}}
{{- if not $entry.host -}}
{{- fail (printf "Values.registries[%d].host is required" $index) -}}
{{- end -}}
{{- if not $entry.remoteUrl -}}
{{- fail (printf "Values.registries[%d].remoteUrl is required" $index) -}}
{{- end -}}
{{- $serviceName := $mirror.serviceName | default (printf "registry-%s" $entry.name) -}}
{{- $portName := $mirror.portName | default $.Values.packageMirror.registry.portName -}}
{{- $port := $mirror.port | default $.Values.packageMirror.registry.port -}}
{{- $debugPortName := $mirror.debugPortName | default $.Values.packageMirror.registry.debugPortName -}}
{{- $debugPort := $mirror.debugPort | default $.Values.packageMirror.registry.debugPort -}}
{{- $proxyUsername := "" -}}
{{- $proxyPassword := "" -}}
{{- with $entry.auth -}}
{{- $decoded := b64dec . -}}
{{- $parts := splitList ":" $decoded -}}
{{- if ge (len $parts) 2 -}}
{{- $proxyUsername = first $parts -}}
{{- $proxyPassword = join ":" (rest $parts) -}}
{{- end -}}
{{- end -}}
{{- $selected = append $selected (dict "name" $entry.name "host" $entry.host "remoteUrl" $entry.remoteUrl "serviceName" $serviceName "portName" $portName "port" $port "debugPortName" $debugPortName "debugPort" $debugPort "enabled" true "proxyUsername" $proxyUsername "proxyPassword" $proxyPassword) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- toYaml $selected -}}
{{- end -}}

{{- define "buildkit-service.packageMirror.registryConfig" -}}
{{- $root := .root -}}
{{- $mirror := .mirror -}}
version: 0.1
log:
  level: {{ $root.Values.packageMirror.registry.env.logLevel | quote }}
storage:
  cache:
    blobdescriptor: inmemory
    blobdescriptorsize: {{ int $root.Values.packageMirror.registry.env.blobDescriptorSize }}
  delete:
    enabled: {{ $root.Values.packageMirror.registry.env.deleteEnabled }}
  filesystem:
    rootdirectory: /var/lib/registry
    maxthreads: {{ int $root.Values.packageMirror.registry.env.filesystemMaxThreads }}
http:
  addr: {{ printf "0.0.0.0:%d" (int $mirror.port) | quote }}
  debug:
    addr: {{ printf "0.0.0.0:%d" (int $mirror.debugPort) | quote }}
    prometheus:
      enabled: true
      path: /metrics
proxy:
  remoteurl: {{ $mirror.remoteUrl | quote }}
  ttl: {{ $root.Values.packageMirror.registry.env.ttl | quote }}
  {{- with (default $root.Values.packageMirror.registry.env.proxyUsername $mirror.proxyUsername) }}
  username: {{ . | quote }}
  {{- end }}
  {{- with (default $root.Values.packageMirror.registry.env.proxyPassword $mirror.proxyPassword) }}
  password: {{ . | quote }}
  {{- end }}
health:
  storagedriver:
    enabled: true
    interval: 10s
    threshold: 3
{{- end -}}
