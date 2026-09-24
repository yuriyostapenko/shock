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
{{- include "shock.validateSecretInjection" . -}}
{{- end -}}

{{/* Effective allow list (JSON array of fqdnEntry): allowedFQDNs, the Trusted
list if enabled, extraAllowedFQDNs; deduped by host:port, minus excludeFQDNs. */}}
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

{{/* "host[:port]" -> (dict "host" "port" "pattern"); pattern spells a leading
"*." as Cilium's multilevel "**.". */}}
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
Secret injection validation (spec section 9). Every failure names the offending
entry; nothing here renders output.
*/}}
{{- define "shock.validateSecretInjection" -}}
{{- $si := .Values.secretInjection -}}
{{- if $si.enabled -}}
{{- if ne .Values.network.mode "cilium" -}}
{{- fail (printf "secretInjection.enabled requires network.mode cilium (got %s): only Cilium can rewrite the header outside the Pod" .Values.network.mode) -}}
{{- end -}}
{{- if empty $si.placeholder -}}
{{- fail "secretInjection.placeholder must not be empty" -}}
{{- end -}}
{{- if not (has $si.pki (list "managed" "existing")) -}}
{{- fail (printf "secretInjection.pki must be managed or existing (got %s)" $si.pki) -}}
{{- end -}}
{{- if empty $si.credentials -}}
{{- fail "secretInjection.enabled with no secretInjection.credentials: nothing would be injected" -}}
{{- end -}}
{{- /* originatingTLS is mandatory: Cilium treats a matched rule with no client
       TLS context as permission to use a raw socket upstream, which would send
       the injected credential in cleartext (spec section 9). Unset, the chart's
       own Mozilla bundle fills it, so the only way to end up without roots is an
       empty bundle file. */ -}}
{{- if not $si.upstreamCA.existingSecret.name -}}
{{- if not (.Files.Get "files/upstream-ca-bundle.pem" | trim) -}}
{{- fail "files/upstream-ca-bundle.pem is missing or empty and secretInjection.upstreamCA.existingSecret is unset: without upstream roots Cilium forwards the intercepted request, credential included, in cleartext. Run `make ca-bundle`, or set your own Secret." -}}
{{- end -}}
{{- end -}}
{{- if eq $si.pki "managed" -}}
{{- if or $si.tls.certificateSecret.name $si.ca.existingConfigMap $si.ca.bundle -}}
{{- fail "secretInjection.pki is managed, so cert-manager issues the certificate: unset secretInjection.tls.certificateSecret and secretInjection.ca, or switch to pki: existing" -}}
{{- end -}}
{{- else -}}
{{- if not $si.tls.certificateSecret.name -}}
{{- fail "secretInjection.pki is existing, so secretInjection.tls.certificateSecret.name is required: Cilium presents it to the runner for every injected host" -}}
{{- end -}}
{{- if and (empty $si.ca.existingConfigMap) (empty $si.ca.bundle) -}}
{{- fail "set secretInjection.ca.existingConfigMap or secretInjection.ca.bundle: the runner must trust the interception CA" -}}
{{- end -}}
{{- if and $si.ca.existingConfigMap $si.ca.bundle -}}
{{- fail "set only one of secretInjection.ca.existingConfigMap and secretInjection.ca.bundle" -}}
{{- end -}}
{{- end -}}
{{- /* Hosts of plain allow-list entries; an injected host must not also be reachable without the proxy. */ -}}
{{- $plain := include "shock.fqdnEntries" . | fromJsonArray -}}
{{- $seenHost := dict -}}
{{- $seenName := dict -}}
{{- range $si.credentials -}}
{{- $c := . -}}
{{- if hasKey $seenName $c.name -}}
{{- fail (printf "secretInjection.credentials: duplicate name %q" $c.name) -}}
{{- end -}}
{{- $_ := set $seenName $c.name true -}}
{{- if hasKey $seenHost $c.host -}}
{{- fail (printf "secretInjection.credentials[%s]: host %q is already injected; one host is one rule" $c.name $c.host) -}}
{{- end -}}
{{- $_ := set $seenHost $c.host true -}}
{{- if eq $c.host "api.anthropic.com" -}}
{{- fail (printf "secretInjection.credentials[%s]: api.anthropic.com can never be intercepted; the control plane, inference and the session's own OAuth token must stay end to end" $c.name) -}}
{{- end -}}
{{- if or (contains "*" $c.host) (contains ":" $c.host) -}}
{{- fail (printf "secretInjection.credentials[%s]: host %q must be an exact FQDN on 443, without wildcard or port" $c.name $c.host) -}}
{{- end -}}
{{- if not $c.secret.name -}}
{{- fail (printf "secretInjection.credentials[%s]: secret.name is required" $c.name) -}}
{{- end -}}
{{- range $plain -}}
{{- if and (contains "*" .host) (hasSuffix (trimPrefix "*" .host) $c.host) -}}
{{- fail (printf "secretInjection.credentials[%s]: host %s is covered by allow-list entry %s, which would admit it on L4 without the proxy; add %s to network.excludeFQDNs" $c.name $c.host .host .host) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /* A client config that names a credential Secret would put it in a ConfigMap. */ -}}
{{- range $file, $body := $si.clientConfigs -}}
{{- $b := $body -}}
{{- range $si.credentials -}}
{{- if contains .secret.name $b -}}
{{- fail (printf "secretInjection.clientConfigs[%s] contains the credential Secret name %q; client configs carry the placeholder only" $file .secret.name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
shock.injectedCredentials: the credentials list with namespaces defaulted to
the release namespace, as a JSON array. Empty when injection is off, so every
consumer can range over it unconditionally.
*/}}
{{- define "shock.injectedCredentials" -}}
{{- $si := .Values.secretInjection -}}
{{- $out := list -}}
{{- if $si.enabled -}}
{{- range $si.credentials -}}
{{- $out = append $out (dict
  "name" .name
  "host" .host
  "header" (default "Authorization" .header)
  "paths" (default (list) .paths)
  "methods" (default (list) .methods)
  "secretNamespace" (default $.Release.Namespace .secret.namespace)
  "secretName" .secret.name
) -}}
{{- end -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}

{{/* Hosts carrying an interception rule; they are dropped from the plain toFQDNs set. */}}
{{- define "shock.injectedHosts" -}}
{{- $out := dict -}}
{{- range include "shock.injectedCredentials" . | fromJsonArray -}}
{{- $_ := set $out .host true -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}

{{/* Names of the cert-manager objects rendered under pki: managed. */}}
{{- define "shock.egressSelfSignedIssuerName" -}}
{{- include "shock.componentName" (dict "root" . "component" "egress-selfsigned") -}}
{{- end -}}
{{- define "shock.egressCAName" -}}
{{- include "shock.componentName" (dict "root" . "component" "egress-ca") -}}
{{- end -}}
{{- define "shock.egressCertName" -}}
{{- include "shock.componentName" (dict "root" . "component" "egress-tls") -}}
{{- end -}}

{{/*
terminatingTLS Secret: the cert-manager leaf under pki: managed, otherwise the
operator's. Namespace + name as JSON.
*/}}
{{- define "shock.terminatingTLSSecret" -}}
{{- $si := .Values.secretInjection -}}
{{- if eq $si.pki "managed" -}}
{{- dict "namespace" .Release.Namespace "name" (include "shock.egressCertName" .) | toJson -}}
{{- else -}}
{{- dict "namespace" (default .Release.Namespace $si.tls.certificateSecret.namespace) "name" $si.tls.certificateSecret.name | toJson -}}
{{- end -}}
{{- end -}}

{{/*
originatingTLS Secret: the operator's override, or the chart's own Secret
rendered from the pinned Mozilla bundle. Namespace + name as JSON.
*/}}
{{- define "shock.upstreamCASecret" -}}
{{- $u := .Values.secretInjection.upstreamCA -}}
{{- if $u.existingSecret.name -}}
{{- dict "namespace" (default .Release.Namespace $u.existingSecret.namespace) "name" $u.existingSecret.name | toJson -}}
{{- else -}}
{{- dict "namespace" .Release.Namespace "name" (include "shock.componentName" (dict "root" . "component" "upstream-ca")) | toJson -}}
{{- end -}}
{{- end -}}

{{/*
How the runner gets the interception CA, as JSON:
  kind      configMap | secret
  name      object name
  namespace only meaningful for the chart's own objects
Under pki: managed it is the issued leaf Secret, from which the volume selects
`ca.crt` alone so no private key is projected (spec section 9).
*/}}
{{- define "shock.egressCASource" -}}
{{- $si := .Values.secretInjection -}}
{{- if eq $si.pki "managed" -}}
{{- dict "kind" "secret" "name" (include "shock.egressCertName" .) | toJson -}}
{{- else if $si.ca.existingConfigMap -}}
{{- dict "kind" "configMap" "name" $si.ca.existingConfigMap | toJson -}}
{{- else -}}
{{- dict "kind" "configMap" "name" (include "shock.componentName" (dict "root" . "component" "egress-ca-bundle")) | toJson -}}
{{- end -}}
{{- end -}}

{{/*
The default runner podTemplate, rendered from values. Helm merges
runner.podTemplate over it (mergeOverwrite: maps merge, lists replace); the
hook then forces the template-contract fields at spawn time.
*/}}
{{- define "shock.runnerPodTemplate" -}}
{{- $r := .Values.runner -}}
{{- $si := .Values.secretInjection -}}
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
      {{- if or $r.extraEnv $si.enabled }}
      env:
        {{- if $si.enabled }}
        {{- /* The entrypoint builds a combined trust bundle from this and exports
               the per-runtime trust variables (spec section 9). */}}
        - name: SHOCK_EGRESS_CA_FILE
          value: /etc/shock/egress-ca/ca.crt
        {{- end }}
        {{- with $r.extraEnv }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
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
        {{- if $si.enabled }}
        - name: egress-ca
          mountPath: /etc/shock/egress-ca
          readOnly: true
        {{- if $si.clientConfigs }}
        - name: registries
          mountPath: /etc/shock/registries
          readOnly: true
        {{- end }}
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
    {{- if $si.enabled }}
    {{- $caSource := include "shock.egressCASource" . | fromJson }}
    {{- /* The CA certificate only. A Secret source is restricted to ca.crt by
           `items`, so no private key is projected; credential Secrets and the
           upstream-roots Secret are referenced by the policy, never by a Pod. */}}
    - name: egress-ca
      {{- if eq $caSource.kind "secret" }}
      secret:
        secretName: {{ $caSource.name }}
        items:
          - key: ca.crt
            path: ca.crt
      {{- else }}
      configMap:
        name: {{ $caSource.name }}
        items:
          - key: ca.crt
            path: ca.crt
      {{- end }}
    {{- if $si.clientConfigs }}
    - name: registries
      configMap:
        name: {{ include "shock.componentName" (dict "root" . "component" "registries") }}
    {{- end }}
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
