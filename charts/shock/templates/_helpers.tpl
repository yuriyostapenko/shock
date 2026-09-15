{{/*
Naming and label helpers. Every template uses these; no template writes a
label literal (spec section 5).
*/}}
{{- define "shock.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* shock.selectorLabels: the frozen subset (name + instance). Pass (dict "root" $ "component" "orchestrator"). */}}
{{- define "shock.selectorLabels" -}}
app.kubernetes.io/name: {{ .component | quote }}
app.kubernetes.io/instance: {{ .root.Release.Name | quote }}
{{- end -}}

{{/* shock.labels: the full common set. Pass (dict "root" $ "component" "runner"). */}}
{{- define "shock.labels" -}}
{{ include "shock.selectorLabels" . }}
app.kubernetes.io/version: {{ default .root.Chart.AppVersion .root.Values.orchestrator.image.tag | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service | quote }}
app.kubernetes.io/part-of: shock
helm.sh/chart: {{ printf "%s-%s" .root.Chart.Name .root.Chart.Version | replace "+" "_" | quote }}
{{- end -}}

{{- define "shock.componentName" -}}
{{- printf "%s-%s" (include "shock.fullname" .root) .component | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "shock.environmentSecretName" -}}
{{- if .Values.environment.existingSecret -}}
{{- .Values.environment.existingSecret -}}
{{- else -}}
{{- include "shock.componentName" (dict "root" . "component" "environment") -}}
{{- end -}}
{{- end -}}

{{/*
shock.image renders repository:tag[@digest]. Pass (dict "image" <image values>
"name" <values key> "defaultTag" <fallback tag>); an empty tag falls back to
defaultTag, and an empty repository or resolved tag fails the render.
*/}}
{{- define "shock.image" -}}
{{- $tag := default .defaultTag .image.tag -}}
{{- if or (empty .image.repository) (empty $tag) -}}
{{- fail (printf "%s.image.repository and %s.image.tag are required" .name .name) -}}
{{- end -}}
{{- $ref := printf "%s:%s" .image.repository $tag -}}
{{- with .image.digest }}{{ $ref = printf "%s@%s" $ref . }}{{ end -}}
{{- $ref -}}
{{- end -}}

{{/* Chart-level validation, evaluated once from the orchestrator Deployment. */}}
{{- define "shock.validate" -}}
{{- $o := .Values.orchestrator -}}
{{- if ge (add $o.hookTimeout 5) (int $o.expectedSpawnSeconds) -}}
{{- fail (printf "orchestrator.hookTimeout (%d) + 5 must be < orchestrator.expectedSpawnSeconds (%d)" (int $o.hookTimeout) (int $o.expectedSpawnSeconds)) -}}
{{- end -}}
{{- if or (lt (int $o.expectedSpawnSeconds) 10) (gt (int $o.expectedSpawnSeconds) 3600) -}}
{{- fail "orchestrator.expectedSpawnSeconds must be within 10..3600" -}}
{{- end -}}
{{- if ne .Values.network.mode "none" -}}
{{- $hasAPI := false -}}
{{- range include "shock.fqdnEntries" . | fromJsonArray }}{{ if eq .host "api.anthropic.com" }}{{ $hasAPI = true }}{{ end }}{{ end -}}
{{- if not $hasAPI -}}
{{- fail "the effective egress allow list (allowedFQDNs, the Trusted list, extraAllowedFQDNs minus excludeFQDNs) must include api.anthropic.com or the runner cannot reach the control plane" -}}
{{- end -}}
{{- end -}}
{{- if and (empty .Values.environment.existingSecret) (empty .Values.environment.secretValue) -}}
{{- fail "set environment.existingSecret (preferred) or environment.secretValue" -}}
{{- end -}}
{{- if and .Values.environment.existingSecret .Values.environment.secretValue -}}
{{- fail "set only one of environment.existingSecret and environment.secretValue" -}}
{{- end -}}
{{- $mount := trimSuffix "/" .Values.runner.storage.mountPath -}}
{{- if and (ne .Values.runner.baseDir $mount) (not (hasPrefix (printf "%s/" $mount) .Values.runner.baseDir)) -}}
{{- fail (printf "runner.baseDir (%s) must be runner.storage.mountPath (%s) or a directory below it" .Values.runner.baseDir .Values.runner.storage.mountPath) -}}
{{- end -}}
{{- if not (has .Values.network.mode (list "cilium" "kubernetes" "none")) -}}
{{- fail "network.mode must be cilium, kubernetes or none" -}}
{{- end -}}
{{- end -}}

{{/*
The effective egress allow list as a JSON array of fqdnEntry dicts, in order:
network.allowedFQDNs, Anthropic's Trusted list when anthropicTrustedDomains is
on, network.extraAllowedFQDNs; then duplicates (same host and port) collapse
and every entry whose host is named in network.excludeFQDNs is dropped.
*/}}
{{- define "shock.fqdnEntries" -}}
{{- $n := .Values.network -}}
{{- $entries := list -}}
{{- range $n.allowedFQDNs }}{{ $entries = append $entries . }}{{ end -}}
{{- if $n.anthropicTrustedDomains -}}
{{- range .Files.Lines "files/anthropic-trusted-domains.txt" -}}
{{- $line := trim . -}}
{{- if and $line (not (hasPrefix "#" $line)) }}{{ $entries = append $entries $line }}{{ end -}}
{{- end -}}
{{- end -}}
{{- range $n.extraAllowedFQDNs }}{{ $entries = append $entries . }}{{ end -}}
{{- $excluded := dict -}}
{{- range $n.excludeFQDNs }}{{ $_ := set $excluded (index (splitList ":" .) 0) true }}{{ end -}}
{{- $out := list -}}
{{- $seen := dict -}}
{{- range $entries -}}
{{- $e := include "shock.fqdnEntry" . | fromJson -}}
{{- $key := printf "%s:%s" $e.host $e.port -}}
{{- if and (not (hasKey $seen $key)) (not (hasKey $excluded $e.host)) -}}
{{- $_ := set $seen $key true -}}
{{- $out = append $out $e -}}
{{- end -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}

{{/*
Parse "host" or "host:port" into (dict "host" "port" "pattern"). "pattern" is
the Cilium spelling: a leading "*." (every subdomain) becomes "**.", which
matches one or more labels in toFQDNs and serverNames alike.
*/}}
{{- define "shock.fqdnEntry" -}}
{{- $parts := splitList ":" . -}}
{{- $host := index $parts 0 -}}
{{- $port := "443" -}}
{{- if gt (len $parts) 1 }}{{ $port = index $parts 1 }}{{ end -}}
{{- $pattern := $host -}}
{{- if hasPrefix "*." $host }}{{ $pattern = printf "*%s" $host }}{{ end -}}
{{- dict "host" $host "port" $port "pattern" $pattern | toJson -}}
{{- end -}}

{{/*
The default runner podTemplate, rendered from values. Helm merges
runner.podTemplate over it (mergeOverwrite: maps merge, lists replace); the
hook then forces the template-contract fields at spawn time.
*/}}
{{- define "shock.runnerPodTemplate" -}}
{{- $r := .Values.runner -}}
metadata:
  labels:
    {{- include "shock.labels" (dict "root" . "component" "runner") | nindent 4 }}
    {{- with $r.podLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  {{- with $r.podAnnotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  terminationGracePeriodSeconds: {{ $r.terminationGracePeriodSeconds }}
  {{- with $r.runtimeClassName }}
  runtimeClassName: {{ . | quote }}
  {{- end }}
  {{- with $r.serviceAccountName }}
  serviceAccountName: {{ . | quote }}
  {{- end }}
  {{- if not (kindIs "invalid" $r.hostUsers) }}
  hostUsers: {{ $r.hostUsers }}
  {{- end }}
  {{- with $r.priorityClassName }}
  priorityClassName: {{ . | quote }}
  {{- end }}
  {{- with $r.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $r.podSecurityContext }}
  securityContext:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $r.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $r.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $r.affinity }}
  affinity:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  containers:
    - name: runner
      image: {{ include "shock.image" (dict "image" $r.image "name" "runner" "defaultTag" .Chart.AppVersion) | quote }}
      imagePullPolicy: {{ $r.image.pullPolicy }}
      {{- if $r.command }}
      command:
        {{- toYaml $r.command | nindent 8 }}
      {{- with $r.args }}
      args:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- else }}
      args:
        - self-hosted-runner
        - --capacity
        - "1"
        - --base-dir
        - {{ $r.baseDir | quote }}
        - --environment-secret-file
        - /var/run/claude/work-order/work-order
        - --release-idle-session-min
        - {{ $r.flags.releaseIdleSessionMin | quote }}
        - --kill-session-after-min
        - {{ $r.flags.killSessionAfterMin | quote }}
        - --exit-if-unused-min
        - {{ $r.flags.exitIfUnusedMin | quote }}
        {{- if $r.flags.pushOutcomeOnRelease }}
        - --push-outcome-on-release
        {{- end }}
        {{- if $r.flags.useAnthropicGitProxy }}
        - --use-anthropic-git-proxy
        {{- end }}
        - --health-port
        - {{ $r.healthPort | quote }}
        {{- range $r.extraArgs }}
        - {{ . | quote }}
        {{- end }}
      {{- end }}
      {{- with $r.extraEnv }}
      env:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      ports:
        - name: health
          containerPort: {{ $r.healthPort }}
          protocol: TCP
      {{- if not $r.command }}
      livenessProbe:
        httpGet:
          path: /healthz
          port: health
        initialDelaySeconds: 30
        periodSeconds: 30
      {{- end }}
      {{- with $r.resources }}
      resources:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $r.securityContext }}
      securityContext:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      volumeMounts:
        - name: workspace
          mountPath: {{ $r.storage.mountPath | quote }}
        - name: work-order
          mountPath: /var/run/claude/work-order
          readOnly: true
        {{- if $r.instructions }}
        - name: instructions
          mountPath: /etc/claude-code/CLAUDE.md
          subPath: CLAUDE.md
          readOnly: true
        {{- end }}
        {{- with $r.extraVolumeMounts }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
  volumes:
    - name: work-order
      secret:
        secretName: shock-placeholder-never-materialized
    {{- if $r.instructions }}
    - name: instructions
      configMap:
        name: {{ include "shock.componentName" (dict "root" . "component" "runner-instructions") }}
    {{- end }}
    {{- with $r.extraVolumes }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
{{- end -}}

{{/* shock.durationSeconds: "5m" | "2h" | "90s" -> seconds. */}}
{{- define "shock.durationSeconds" -}}
{{- $s := toString . -}}
{{- if hasSuffix "h" $s }}{{ mul (trimSuffix "h" $s | int) 3600 }}
{{- else if hasSuffix "m" $s }}{{ mul (trimSuffix "m" $s | int) 60 }}
{{- else if hasSuffix "s" $s }}{{ trimSuffix "s" $s | int }}
{{- else }}{{ $s | int }}{{ end -}}
{{- end -}}
