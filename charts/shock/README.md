# shock

Self-Hosted Orchestrator for Claude on Kubernetes. One chart installs:

- the Anthropic orchestrator (`claude self-hosted-runner orchestrator`) with the
  `spawn-runner` hook baked into its image,
- the SHOCK session controller, which sequences sleep, wake, spawn observation,
  idle garbage collection and stranded-session alarms,
- the Helm-rendered Sandbox template the hook stamps per session,
- RBAC, network policy and monitoring for all of the above.

Every Claude session gets its own `Sandbox` and a persistent workspace PVC that
survives sleep. Runner compute scales to zero between messages.

## Prerequisites

Required:

| Requirement | Notes |
| --- | --- |
| Kubernetes `>= 1.35` | Derived from agent-sandbox v1.0.2's `k8s.io/*` v0.37 pin minus two minors. Older clusters may work but are untested and unsupported. |
| [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) controller, tested range **v1.0.2** | Install from the upstream release manifests. This chart never renders its CRDs or controller. Until the CRD is served the session controller stays alive but not ready and the hook exits 1 (retryable). |
| A Claude self-hosted environment | Create it on the Cloud environments admin page and store the environment key in a Secret (key `environment-secret`). |
| Persistent storage for per-session PVCs | Default access mode `ReadWriteOncePod` (needs a CSI driver). Immutable per session after creation. |
| The SHOCK image and a runner image | Both published by the release; see below. |

Optional, recommended:

| Component | Why | Without it |
| --- | --- | --- |
| [Cilium](https://cilium.io) (default `network.mode: cilium`) | Default-deny egress by host name with TLS SNI enforcement; the only mode that limits runners to an allow list. Tested with 1.20. | `network.mode: kubernetes` renders plain NetworkPolicy (ports only, any address), `none` renders nothing. |
| [Prometheus Operator](https://prometheus-operator.dev) (default `monitoring.enabled: true`) | PodMonitor scraping all three components and the alert rules for stale polls, failed spawns and stranded sessions. | Set `monitoring.enabled: false`, or the install fails on the missing CRDs. |
| [cert-manager](https://cert-manager.io), only with `secretInjection.enabled` | Issues the interception CA and the certificate Cilium presents, under the default `secretInjection.pki: managed`. | Set `secretInjection.pki: existing` and supply the certificate yourself, or the install fails on the missing CRDs. |

## Install

Releases publish the chart as an OCI artifact at
`oci://ghcr.io/yuriyostapenko/charts/shock` and the image at
`ghcr.io/yuriyostapenko/shock`, both tagged `X.Y.Z` from the git tag `vX.Y.Z`.
The released chart's `appVersion` is that same `X.Y.Z` and its default values
pin the released image by digest, so the orchestrator image needs no values.

```sh
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.2/sandbox.yaml
kubectl create namespace claude-runners
(umask 077 && cat > ./environment-secret)   # paste the environment key, Enter, Ctrl-D
kubectl -n claude-runners create secret generic claude-environment --from-file=environment-secret=./environment-secret && rm ./environment-secret

helm install shock oci://ghcr.io/yuriyostapenko/charts/shock --version X.Y.Z -n claude-runners \
  --set environment.existingSecret=claude-environment
```

From a checkout, `Chart.yaml` carries `0.0.0-dev`; pass `orchestrator.image.tag`
and `runner.image.tag` (and optionally the digests) yourself.

Both artifacts are signed keyless with cosign and carry GitHub build provenance:

```sh
cosign verify ghcr.io/yuriyostapenko/shock:X.Y.Z \
  --certificate-identity-regexp '^https://github.com/yuriyostapenko/shock/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/yuriyostapenko/charts/shock:X.Y.Z --repo yuriyostapenko/shock
```

`helm template` and `helm install` both succeed with the Sandbox CRD absent, so
GitOps engines can converge in any order.

## Images

**SHOCK image** (`orchestrator.image`, built from `images/orchestrator/Dockerfile`):
`gcr.io/distroless/base-debian13:nonroot` carrying `claude` for the orchestrator
process, the `shock` binary for the hook and the session controller, and
`/hooks/spawn-runner` as a symlink to `shock`, which runs the hook when invoked
under that name. The image has no shell or package manager and runs as user
65532. The orchestrator Deployment leaves `/hooks` to the image.

**Default runner image** (`runner.image`, built from `images/runner/Dockerfile`,
published as `ghcr.io/yuriyostapenko/shock-runner:X.Y.Z` and pinned by digest
in the released chart). It follows Anthropic's recipe: `debian:trixie-slim`,
`git` 2.47 and `openssh-client`, the native `claude` binary verified against
the release manifest, `libicu76` and `LANG=C.UTF-8` (ICU and a UTF-8 locale,
which .NET and other runtimes installed later expect), `procps` (`ps`, `pgrep`,
`pkill`, used by Claude Code and by scripts managing background processes),
and the doc's system git configuration. User `runner`
(uid 1000, the chart's default `fsGroup`) owns its home, `/home/runner`, and
two user-space package managers are on `PATH` so sessions can install tooling
without root:

- `mise` installs language runtimes and CLIs into `~/.local/share/mise`
  (`mise use -g node@22`, `mise use -g go@latest`, `mise use -g jq`).
- `uv` and `uvx` install Python versions and Python tools into `~/.local`.

The image entrypoint sets `CLAUDE_CODE_TMPDIR` to `~/.cache/claude-tmp`
(unless already set) before starting the CLI, so the scratchpad and task state
persist across sleep like the rest of the home.

The chart mounts the session's PVC at the runner user's home
(`runner.storage.mountPath`, default `/home/runner`) and checks repositories out
below it (`runner.baseDir`, default `/home/runner/workspace`). Everything a
session installs or configures under `~` therefore survives sleep and is already
there on resume: `mise` runtimes, `uv` tools and interpreters, the npm cache,
and any other dotfile. The runner's hard reset on resume touches only the
repository directory. Two consequences: the storage class must not mount volumes
`noexec`, since runtimes execute from the disk; and credentials that tools cache
in dotfiles (`~/.npmrc`, `~/.netrc`, `~/.docker/config.json`) stay on the disk
until the session is garbage-collected, so prefer short-lived credentials from a
wrapper. With the git proxy on, the runner wipes `~/.gitconfig` and
`~/.config/git` at startup.

`apt` is present but needs root. On clusters that support Pod user namespaces
(Kubernetes 1.36 GA; containerd 2.0+ or CRI-O 1.25+, kernel 6.3+ with
idmapped mounts), set `runner.hostUsers: false` together with
`runner.securityContext.runAsUser: 0` and `runAsNonRoot: false`: root inside
the container is then an unprivileged host user, `apt-get install` works, and
Pod Security Standards waive the non-root requirement for such Pods.

**Bring your own runner image** with `FROM ghcr.io/yuriyostapenko/shock-runner:X.Y.Z`
and add toolchains as root before switching back to `USER 1000`. Whatever the
image, the contract is: `claude` at 2.1.224 or later, pinned; `git >= 2.32`; a
non-root user whose `$HOME` is `runner.storage.mountPath`, the PVC, with
`runner.baseDir` inside it; and, optionally, a credentials wrapper wired with
`runner.extraArgs: ["--exec-path", "/opt/claude/wrapper.sh"]`. With
`secretInjection` on the image must also trust the interception CA; the default
entrypoint does that from `SHOCK_EGRESS_CA_FILE` (see
[Certificates](#certificates)).

## How a session runs

1. The orchestrator claims a spawn request and runs `/hooks/spawn-runner`. The
   hook strict-decodes `/etc/shock/sandbox-template.yaml`, stamps the session
   identity and forces the template contract, and creates the Sandbox
   **Suspended** with `pending-spawn`, `pending-spawn-at`, `last-order-id` and
   `last-order-attempt` in the create body. It then creates the immutable
   work-order Secret owned by the Sandbox UID and records `pending-secret`.
   It exits without waiting for anything.
2. The session controller sees a Suspended Sandbox with current-generation
   `Suspended=True` and a pending order, validates the Secret against order,
   attempt, session and Sandbox UID, and in one conditional patch sets
   `operatingMode: Running`, `applied-spawn` and the Pod template's order and
   Secret reference.
3. agent-sandbox creates the Pod and mounts `workspace-<sandbox>`. The controller
   observes an owned Pod carrying the order and clears pending intent. Readiness
   is never consulted.
4. The runner exits when the session is released or idle. agent-sandbox reports
   `Finished=True`; the controller patches `operatingMode: Suspended` and stamps
   `last-suspended-at`. The Pod is deleted; the PVC and Sandbox stay.
5. A later spawn request for the same session finds the Sandbox, publishes a new
   immutable Secret and pending order, requests suspension, and the controller
   wakes it once suspension is confirmed. The canonical clone is already on
   disk, so the runner does a fetch and hard reset rather than a fresh clone.
6. After `sessionController.gc.maxIdleAge` asleep, or when more than
   `sessionController.gc.maxIdleSessions` are asleep and this is among the
   oldest, the Sandbox is deleted with UID and resourceVersion preconditions.
   The PVC and every work-order Secret cascade by ownerReference. Recreating
   the session later starts with a new disk. The count cap reaches older
   Sandboxes on their next resync (`sessionController.resyncSeconds`).

The Sandbox `operatingMode` alone is never trusted: every lifecycle decision
also requires the relevant condition to be True for the current
`metadata.generation`, so a stale condition or a pod still terminating cannot
produce two pods on one PVC.

### Bounce against a live session

A new spawn request against a still-running session patches `operatingMode:
Suspended`; the old Pod drains (`Suspended=False/PodTerminating`) and only after
`Suspended=True` does Wake install the new order. The pod count never exceeds
one. The hook never rotates a running Pod's JWT.

### Active-session cap

`orchestrator.maxActiveSessions` (default 2, `0` disables) bounds sessions per
release. The hook lists the release's Sandboxes before creating one or
accepting a newer order and counts those
`Running` or holding a pending order. At the cap it exits 1 with "at capacity"
on stderr: nothing is created, the control plane shows that reason in the
Activity tab and re-offers the session after its own backoff. Redelivery and
a bounce onto a session that already holds a slot pass. The count is a
snapshot, so concurrent hooks can overshoot by up to `hookConcurrency` per
replica; set `hookConcurrency: 1` for a tighter bound. The session controller
plays no part; sleeping Sandboxes never count.

### Template changes reach sessions at their next spawn

When the hook accepts a higher attempt for an existing Sandbox it also installs
the currently rendered pod template (image, flags, resources), carrying over
the order currently on the Pod. A `helm upgrade` therefore never touches
sleeping Sandboxes; they pick the new template up on their next spawn.
`volumeClaimTemplates` is immutable: storage changes apply to new sessions only.

## Values

See `values.yaml` for the full, commented list; `values.schema.json` enforces
types and enums. The load-bearing ones:

| Key | Default | Notes |
| --- | --- | --- |
| `environment.existingSecret` | `""` | Secret with key `environment-secret`. Required unless `secretValue` is set. |
| `orchestrator.image.tag`, `runner.image.tag` | `""` | Fall back to the chart's `appVersion`. The `digest` fields pin the images; the release sets them. |
| `runner.hostUsers` | unset | `false` runs the runner Pod in a user namespace so root inside the container can use `apt`. |
| `secretInjection.enabled` | `false` | Inject credentials into runner egress without the session holding them; needs `network.mode: cilium`. See [Secret injection](#secret-injection). |
| `secretInjection.pki` | `managed` | `managed` has cert-manager issue the interception CA and certificate; `existing` takes your own references. |
| `secretInjection.upstreamCA.existingSecret` | none | Overrides the public roots the chart ships for `originatingTLS`. |
| `runner.flags.useAnthropicGitProxy` | `true` | Git goes through `api.anthropic.com` with the session creator's GitHub connection; the runner holds no git credentials. Set `false` when supplying credentials yourself. |
| `orchestrator.expectedSpawnSeconds` | `180` | Server-side spawn lease, shared by all replicas. Must exceed `hookTimeout + 5` (rendering fails otherwise). Includes the initial suspension round-trip. |
| `orchestrator.hookTimeout` | `30` | The hook keeps its API work within 80% of this. |
| `orchestrator.maxActiveSessions` | `2` | Sessions running or waiting to start in this release. Beyond it a new session's hook exits 1: the user sees "at capacity" as the reason and the control plane re-offers the session on its own backoff. `0` = unlimited. |
| `sessionController.gc.maxIdleAge` | `336h` | Sandbox, PVC and Secrets are deleted after 14 days asleep. |
| `sessionController.gc.maxIdleSessions` | `10` | Asleep Sandboxes kept per release; beyond it the oldest by `last-suspended-at` are deleted. Bounds disk. `0` = unlimited. |
| `sessionController.zombie.alertAfter` | `5m` | Pods Terminating longer than this raise an Event and alert. SHOCK never force-deletes. |
| `runner.storage.mountPath` | `/home/runner` | Where the per-session PVC is mounted: the runner user's home. |
| `runner.baseDir` | `/home/runner/workspace` | The runner's `--base-dir`, at or below the mount path. Same on every runner. |
| `runner.terminationGracePeriodSeconds` | `120` | Runner SIGKILL floor is 75 s at defaults, 105 s with push-outcome. |
| `runner.resources` | 2 CPU / 4Gi requested, 4 CPU / 4Gi limits | Anthropic's starting values for one session ([sizing](https://code.claude.com/docs/en/self-hosted-environments-deploy#size-cpu-and-memory-for-sessions)): memory request equals limit, CPU bursts for builds. Measure a representative build and raise. |
| `orchestrator.resources`, `sessionController.resources` | 100m / 256Mi and 50m / 128Mi requested, 512Mi and 256Mi limits | Sized from live measurements (~150Mi and <50Mi steady state); CPU is negligible. No CPU limits, so hooks and reconciles are never throttled. |
| `runner.storage.accessMode` | `ReadWriteOncePod` | Immutable per session. Decide before first install. |
| `runner.instructions` | environment notes | Markdown mounted at `/etc/claude-code/CLAUDE.md` in every runner Pod, Claude Code's managed-policy instructions: what Claude should know about this runner (persistent home, root-free installs, git proxy). Empty mounts nothing. |
| `runner.podTemplate` | `{}` | Deep-merged over the rendered pod template (maps merge, lists replace). |
| `network.allowedFQDNs` | hosts the Trusted list lacks: Anthropic downloads and docs, GitHub asset and ghcr layer hosts, mise metadata, Go and .NET downloads, registry layer hosts | `host` or `host:port`; a leading `*.` matches every subdomain. This deployment's own list, disjoint from Anthropic's; see values.yaml. |
| `network.anthropicTrustedDomains` | `true` | Also allow Anthropic's Trusted-level default domains from `files/anthropic-trusted-domains.txt`. |
| `network.extraAllowedFQDNs` | `[]` | Appended to the merged list: add hosts without copying the defaults. |
| `network.excludeFQDNs` | `[]` | Hosts dropped from the merged list, written as they appear in it. |
| `network.enforceSNI` | `true` | cilium mode: TLS to an `allowedFQDNs` entry must carry a matching SNI; wildcard entries are enforced as patterns. |
| `network.mode` | `cilium` | `kubernetes` renders plain NetworkPolicy without FQDN rules; `none` renders nothing. |

### `runner.podTemplate` and the template contract

Helm merges `runner.podTemplate` over the rendered template; the hook then
forces these fields at spawn time, so an overlay cannot break the lifecycle:

| Field | Forced to |
| --- | --- |
| `spec.restartPolicy` | `Never` |
| `spec.automountServiceAccountToken` | `false` |
| `spec.terminationGracePeriodSeconds` | `runner.terminationGracePeriodSeconds` |
| `metadata.labels` | the common and session label set, merged last |
| `runner` container `workspace` mount path | `runner.storage.mountPath`; `runner.baseDir` must be at or below it |
| `runner` container `work-order` mount | `/var/run/claude/work-order`, read-only |
| `spec.operatingMode` on creation | `Suspended` |

Two anchors are required and must keep their names: the container `runner`
and the volume `work-order`. Removing or renaming either makes the hook exit 2
with a message naming the anchor, before anything is written. Users keep
everything else: resources, nodeSelector, tolerations, affinity,
priorityClassName, imagePullSecrets, serviceAccountName, securityContext, extra
labels, annotations, volumes and mounts. Because lists replace on merge, prefer
the dedicated values (`runner.resources`, `runner.extraEnv`, `runner.extraVolumes`,
`runner.extraVolumeMounts`, `runner.extraArgs`) over overriding `containers`.

## Namespace, names and RBAC

- Dedicate the namespace to SHOCK. The hook's Role grants `secrets create` and
  `sandboxes list/create/patch` namespace-wide because RBAC cannot prefix-match names.
- Sandbox names are `<release>-cs-<sanitized session id>` (RFC 1123, at most 63
  chars, plus an 8-char hash when sanitizing or truncating changed the id), so
  releases never collide on names. A session still belongs to exactly one
  release: two releases serving the same Anthropic environment in one namespace
  is a misconfiguration and the hook refuses a Sandbox labeled for another
  release with exit 2. Two releases serving different environments can share a
  namespace; every selector includes `app.kubernetes.io/instance`.
- Work-order Secrets are `<release>-wo-<sha256(release, session, Sandbox UID, order)>`,
  immutable, owned by the Sandbox (non-controller, `blockOwnerDeletion: false`).
  One Secret per attempted order lives until the Sandbox is collected; size
  Secret quotas accordingly. A recreated Sandbox derives different names and
  cannot adopt old credentials.
- The session controller can `patch` and (with GC enabled) `delete` Sandboxes,
  `get` Secrets and `get/list/watch` Pods. It never deletes Pods or Secrets.
- Runner Pods never receive a service-account token.

## Registry and API credentials

Three routes, in the order to prefer them:

1. **Pull-through mirror** for public registries. Bake mirror URLs into the
   runner image or inject them with `runner.extraEnv`. Add the mirror host to
   `network.allowedFQDNs`; otherwise installs hang against default-deny egress
   and fail on timeout instead of a clear error. No credential at all.
2. **Secret injection** (below) for anything whose credential travels in an
   HTTP header: GitHub Packages, a private registry, an internal API. The
   session never holds the credential.
3. **Wrapper** for everything else: SSH keys, database passwords, cloud STS
   sessions. `runner.extraArgs: ["--exec-path", "/opt/claude/wrapper.sh"]`;
   mount the Secret with `runner.extraVolumes` and `runner.extraVolumeMounts`.
   The wrapper materializes what it needs before
   `exec "$CLAUDE_RUNNER_CLAUDE_BIN" "$@"`. This one is agent-visible by
   construction: whatever the wrapper writes, the session can read. Mint it per
   session and scope it to the session creator with the session JWT
   (`self-hosted-runner decode-token`). Never bake broad tokens into the shared
   image, which every session of every member runs.

## Secret injection

`secretInjection` gives sessions credentials they never hold. Cilium's
node-local Envoy terminates the runner's TLS connection to each listed host,
sets the bound header from a Kubernetes Secret, and re-originates to the real
host. The credential is never in the Pod: not in an environment variable, a
mount, the workspace disk or the Pod spec. The session sees a placeholder. This
is the self-hosted counterpart of the API credentials Anthropic-hosted
environments offer, which a self-hosted environment does not have.

It needs `network.mode: cilium` and Cilium 1.17 or later with its L7 proxy and
policy-secret sync, plus cert-manager for the default `pki: managed`. The chart
refuses to render otherwise.

```yaml
secretInjection:
  enabled: true
  # pki: managed is the default: cert-manager issues the interception CA and
  # the certificate Cilium presents, and the chart ships the public roots
  # Cilium verifies the real host against. See Certificates below.
  credentials:
    - name: ghcr
      host: ghcr.io
      secret: {namespace: shock-credentials, name: ghcr-pat}
      paths: ["/token(\\?.*)?"]       # the OCI token endpoint only
    - name: npm-pkg
      host: npm.pkg.github.com
      secret: {namespace: shock-credentials, name: npm-pat}
  clientConfigs:
    npmrc: |
      //npm.pkg.github.com/:_authToken=proxy-injected
```

### What it protects and what it does not

- **The header is replaced, not matched.** Whatever the session sends in the
  bound header on a matching request, Envoy overwrites it; a missing header is
  added. The placeholder is a convention for client configs and for the agent,
  not a gate. So **every process in the session can use the credential against
  the bound host and paths**, the same property Anthropic's API credentials
  have. Bind narrowly: one host, the least scope the registry offers, and a
  path where the protocol allows one.
- **Only listed hosts are intercepted.** Everything else keeps end-to-end TLS
  with the SNI check below. `api.anthropic.com` can never be injected; the
  render fails, because the control plane, inference and the session's own
  OAuth token must stay untouched.
- **A derived token still enters the Pod.** `ghcr.io` exchanges `Basic`
  credentials on `GET /token` for a short-lived, repository-scoped `Bearer`
  JWT, which the client then presents from inside the Pod on `/v2/...`. Scope
  the credential to `/token` so the token itself never leaves the node.
- **A tool that does not trust the CA fails loudly**, with a certificate error
  on injected hosts only. Nothing is sent in the clear. The one configuration
  that would leak is an interception rule without upstream roots, which the
  chart cannot render; see [Certificates](#certificates).

### Put the Secrets where SHOCK cannot read them

The orchestrator and session-controller Roles hold `secrets get` in the release
namespace, and Kubernetes RBAC cannot match names by prefix, so a credential
Secret in the release namespace is readable by both. Cilium reads policy
Secrets from any namespace, so use a dedicated one. The upstream roots are
public and can live anywhere; only the credentials need this:

```sh
kubectl create namespace shock-credentials

# One key, whose value is the whole header value.
kubectl -n shock-credentials create secret generic npm-pat \
  --from-literal=value="Bearer $GITHUB_PAT"

# Basic takes user:token, base64 encoded, with the scheme in front.
kubectl -n shock-credentials create secret generic ghcr-pat \
  --from-literal=value="Basic $(printf '%s' "$GITHUB_USER:$GITHUB_PAT" | base64 -w0)"
```

GitHub Packages accepts only a classic personal access token; `read:packages`
is the scope to grant. Header encoding per registry:

| Host | Header value | Scope |
| --- | --- | --- |
| `npm.pkg.github.com` | `Bearer <PAT>` | every request |
| `nuget.pkg.github.com` | `Basic <base64 user:PAT>` | every request |
| `maven.pkg.github.com` | `Basic <base64 user:PAT>` | every request |
| `ghcr.io` | `Basic <base64 user:PAT>` | `paths: ["/token(\\?.*)?"]` |

### Certificates

Interception needs three pieces of PKI. Two of them the chart can own; the third
it deliberately will not.

**The certificate Cilium presents, and the CA behind it.** With the default
`secretInjection.pki: managed` the chart renders four namespaced cert-manager
objects: a self-signed `Issuer`, a CA `Certificate`, a CA `Issuer`, and a leaf
`Certificate` whose SANs are exactly the injected hosts. There is nothing to
prepare and nothing to rotate. Adding a host to `credentials` reissues the
certificate with the new SAN. cert-manager becomes a prerequisite, the way
Cilium already is for `network.mode: cilium`.

The runner trusts that CA through the issued Secret's `ca.crt`, mounted with an
`items` selector naming that key alone, so the leaf private key is never
projected into the session and the CA key's Secret is referenced by no Pod at
all.

Set `pki: existing` to use your own PKI instead. Then
`tls.certificateSecret` must name a `kubernetes.io/tls` Secret with a SAN for
every injected host, and `ca.existingConfigMap` or `ca.bundle` must carry that
CA's public certificate. The chart renders no cert-manager objects in this mode,
and refuses to render if a `pki: managed` field is also set, so a leftover value
cannot look effective.

**The public roots Cilium verifies the real host against.** The chart ships
these, so there is nothing to set. `charts/shock/files/upstream-ca-bundle.pem`
is Mozilla's CA list exactly as `curl.se` publishes it, fetched by
`make ca-bundle` and verified against the digest `curl.se` publishes beside it.
That digest is recorded in the file's own header, `make ca-bundle-check`
re-verifies it offline, and CI runs that check on every push, so a file edited
by hand fails the build. You can verify it yourself: the recorded digest is
published at `https://curl.se/ca/cacert.pem.sha256`.

To use your own roots instead, for an internal PKI or a vetted list, point the
chart at a Secret with key `ca.crt`:

```yaml
secretInjection:
  upstreamCA:
    existingSecret: {namespace: shock-credentials, name: upstream-roots}
```

If you already run [trust-manager](https://cert-manager.io/docs/trust/trust-manager/),
its `Bundle` is one way to produce that Secret. The chart does not render the
Bundle for you: it is cluster-scoped, and a Secret target needs trust-manager's
`secretTargets.enabled`, which widens its access to Secrets. Apply it yourself
and point `existingSecret` at the result:

```yaml
apiVersion: trust.cert-manager.io/v1alpha1
kind: Bundle
metadata: {name: shock-upstream-roots}
spec:
  sources: [{useDefaultCAs: true}]
  target:
    secret: {key: ca.crt}
    namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: <your namespace>}}
```

Deriving the roots from a machine's own trust store works too, with one caveat
worth stating plainly: if that machine sits behind a TLS-inspecting proxy, its
store holds that proxy's CA and you would be teaching Cilium to accept
certificates that proxy issues. Check what you are about to load.

**Upstream roots are never optional**, and that is a safety property rather
than tidiness. Cilium treats a matched rule carrying no `originatingTLS` as
permission to use a raw socket upstream, so a rule with only `terminatingTLS`
would have Cilium decrypt the runner's request and forward it, injected
credential included, in cleartext. Every rule the chart renders carries
`originatingTLS`, and the render fails if the shipped roots file is missing or
empty and you have set no override.

### Client configs and the agent

`clientConfigs` becomes a ConfigMap mounted read-only at
`/etc/shock/registries`. The files carry the placeholder, so they hold no
secret, and the render fails if one names a credential Secret. Point tools at
them through `runner.extraEnv`, for example
`NPM_CONFIG_GLOBALCONFIG: /etc/shock/registries/npmrc` or
`DOCKER_CONFIG: /etc/shock/registries/docker`.

With injection on, the managed instructions the runner loads
(`/etc/claude-code/CLAUDE.md`) gain a generated block naming the injected hosts,
their path scope and the placeholder, so the agent uses `proxy-injected`
instead of hunting for a token it cannot find. The block names hosts only,
never a Secret.

### Cilium settings

Policy Secrets must reach Envoy. The SDS defaults of a new Cilium install are
what the chart expects:

```yaml
tls:
  secretSync: {enabled: true}
  readSecretsOnlyFromSecretsNamespace: true
```

The Cilium operator then copies the Secrets this chart references into
`cilium-secrets` and serves them to Envoy by reference, so the Cilium agent
needs no cluster-wide Secret read. A cluster upgraded with
`upgradeCompatibility` at 1.16 or below runs the legacy mode instead, without
sync; there, create the Secrets directly in `cilium-secrets` and point
`secretInjection` at that namespace.

If you run Hubble, redact the injected headers. Cilium logs a header mismatch
in its L7 access log, and that log carries the expected value:

```yaml
hubble:
  redact:
    enabled: true
    http:
      userInfo: true
      headers:
        deny: [Authorization, Proxy-Authorization]
```

`network.mode: kubernetes` and `none` cannot inject: plain NetworkPolicy has no
L7 proxy and no TLS interception.

## Network policy

`cilium` mode renders one `CiliumNetworkPolicy` per component, selected by the
release-scoped selector labels: default-deny egress, DNS to kube-dns, `toFQDNs`
for every `network.allowedFQDNs` entry on its port (443 default), an explicit
L3 deny for `169.254.169.254/32`, and `kube-apiserver` for the orchestrator and
session controller. This governs egress from the Pod; image pulls are the
kubelet's traffic and do not belong here.

Two lists feed the runner's `toFQDNs` rules. `network.allowedFQDNs` is this
deployment's own list. `network.anthropicTrustedDomains` (default `true`) adds
Anthropic's Trusted-level default domains, the allow list of Anthropic-hosted
environments, kept verbatim in `files/anthropic-trusted-domains.txt` with its
source and fetch date so it can be refreshed with `make trusted-domains`
without touching your list. It is broad (`*.amazonaws.com`, `*.googleapis.com`
and other object stores are on it); set it to `false` for a default-deny
posture that allows only what you list. The default `allowedFQDNs` holds only
what Anthropic's list lacks, so with the Trusted list off you list every host
yourself.

Customize without copying: `network.extraAllowedFQDNs` appends (your git host,
a mirror, internal services) and `network.excludeFQDNs` removes entries from
the merged result (`*.amazonaws.com` to keep the Trusted list but not object
stores). Overriding `network.allowedFQDNs` replaces the chart's own list, which
is Helm's normal list behaviour. Order: `allowedFQDNs`, the Trusted list,
`extraAllowedFQDNs`, then dedupe, then excludes. Excludes act on entries, not
reach: removing `storage.googleapis.com` changes nothing while
`*.googleapis.com` is still listed. Whatever the combination, the chart refuses
to render unless `api.anthropic.com` remains in the effective list.

`toFQDNs` admits the addresses a name resolved to, and CDN addresses are shared
between customers: a host that lands on the same address as an allowed one is
reachable by address. With `network.enforceSNI` (default) every entry also
carries `serverNames` with the same name or `*` pattern, so the TLS handshake
must name a matching host.

`kubernetes` mode renders plain `NetworkPolicy`: DNS plus any address on the
allowed ports except the metadata CIDR. There is no FQDN filtering; enforce host
allowlists at your network boundary.

## Monitoring

A `PodMonitor` scrapes the named `health` port at `/metrics` on orchestrator,
session controller and runner pods of this release. It drops the `email` label
from `claude_code_self_hosted_runner_locked_account` (PII) by default.

The session controller exports sandbox state from its informer cache, labeled by
Sandbox name only (no session or account ids):

| Series | Meaning |
| --- | --- |
| `shock_sandbox_condition_status{sandbox,type,reason,operating_mode,current}` | 1 True, 0 False, -1 Unknown; `current` is whether observedGeneration matches generation |
| `shock_sandbox_pending_spawn_age_seconds{sandbox}` | age of an unobserved order |
| `shock_sandbox_pod_terminating_seconds{sandbox,pod}` | stranded-pod detection |
| `shock_sandbox_info{sandbox,operating_mode,pending}`, `shock_sandboxes{operating_mode}` | inventory |
| `shock_crd_served` | 1 while the Sandbox CRD is served |
| `shock_build_info{version,revision,go_version}` | always 1; the git tag or pseudo-version and commit the binary was built from |

The `PrometheusRule` carries Anthropic's sample alerts (runner poll stale,
orchestrator disconnected, poll stale above `hookTimeout + 30`, circuit-broken,
spawn-hook failures) and SHOCK's: sleep transition failed (`Finished=True` while
Running for 2 min), MultiplePods, pending spawn older than
`expectedSpawnSeconds`, session controller not ready or erroring, and session
stranded (Pod Terminating beyond `zombie.alertAfter`). The last one needs a
cluster admin: SHOCK never taints nodes or force-deletes Pods. Unless the
active-session cap is `0`, the spawn-hook alert counts only
non-retryable results, since exit 1 is then a capacity hold; the info-level
`ClaudeSessionsBackingOff` fires when sessions stay in backoff longer than
`monitoring.prometheusRule.backingOffFor`.

Wake-latency SLO query:

```promql
histogram_quantile(0.99, sum by (le) (rate(claude_code_self_hosted_orchestrator_session_queue_wait_seconds_bucket[15m])))
  + histogram_quantile(0.99, sum by (le) (rate(claude_code_self_hosted_runner_session_init_duration_seconds_bucket[15m])))
```

Probes on `/healthz` detect a dead process only; the orchestrator's poll health
is `claude_code_self_hosted_orchestrator_connected`.

## Operations

- **Stranded session** (`ShockSessionStranded`): a Pod is stuck Terminating,
  usually on an unhealthy node. Drain or recover the node. Do not force-delete
  the Pod: it defeats the `Suspended=True` gate and the deterministic Pod name
  and is the only way to get two Pods on one PVC.
- **Circuit-broken sessions**: the hook exited 2 (template, RBAC, identity or
  immutable-Secret mismatch). The stderr tail shows in the environment's
  Activity tab. Fix the cause, then Retry there.
- **Upgrading agent-sandbox**: only versions in the e2e matrix are tested.
  Re-derive `kubeVersion`, re-run the conformance suite, and confirm no
  controller-driven idle-suspend applies by default before adding a version.
