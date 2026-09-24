# Working on SHOCK

## Start here

- Read `README.md` and all of `plans/shock-spec.md` before implementation.
- The implementation is in place: `charts/shock`, `cmd/shock`, `internal/*`,
  `images/*`, `hack/*`, `test/*` and the Makefile. Run `make all` before and after
  a change; lifecycle changes also need `make envtest` and `make e2e-kind`.
- Follow explicit user instructions. Otherwise, treat the spec as the implementation
  contract; resolve contradictions explicitly and update related sections together.
- Check the spec's verification items against pinned upstream releases before relying
  on beta protocol behavior. Record dependency versions and relevant evidence in the
  verification record at the end of the spec's section 13.

## Scope and structure

- Use Go for the shared `shock` binary, with `hook spawn-runner` and
  `session-controller` subcommands. Follow the layout in spec section 3; the one
  addition is `internal/patch` (UID + resourceVersion pinned merge patches).
- Keep this chart namespaced. agent-sandbox is an external prerequisite; do not vendor
  its CRDs or install its controller through this chart.
- Keep the spawn hook fast: no waits or polling for Pods or lifecycle conditions.
- Do not add unrelated features or speculative abstractions during scaffolding.
- Do not spawn sub-agents unless the user explicitly requests delegation.

## Lifecycle invariants

- Every session has its own persistent workspace; sleep preserves that workspace.
- Use immutable, owned per-order Secrets. Never log JWTs, environment secrets, or
  account email addresses; never commit credentials or real work-order fixtures.
- Enforce UID/resourceVersion preconditions for lifecycle mutations and GC. Re-read
  and recompute on conflicts; never force stale intent onto a newer object.
- Condition truth alone is insufficient: lifecycle decisions must check generation
  freshness and matching intent as specified. Readiness is observational only.
- Correlate spawn acknowledgement with the actual owned Pod's order and Secret.
- Never force-delete Pods or mutate Nodes. Stranded sessions require an alert and
  cluster-admin intervention.
- Runner Pods must not receive service-account tokens.

## Full-cycle work on a live cluster

The kind cluster from `make e2e-setup` doubles as the live test bed. Reuse it;
never delete a cluster, namespace or release you did not create in the current
session unless asked.

- Images: `make image` and `make image-runner` build `shock:dev` and
  `shock-runner:dev` from the working tree; `kind load docker-image <ref> --name
  shock-e2e` puts them on the node. Verify the binary with `docker run --rm
  --entrypoint /usr/local/bin/shock shock:dev version`.
- Release: `helm upgrade --install shock-live charts/shock -n shock-live -f
  hack/live-values.example.yaml` (copy and adjust). The environment key lives in a
  Secret the operator creates out of band; it never appears in values, logs, commits
  or the transcript.
- Reloading an image under an unchanged tag rolls nothing out. After `kind load`,
  `kubectl rollout restart` the orchestrator and the session controller together:
  the hook and the controller derive the same names and must run the same build.
  A runner Pod only picks up a new image, template or ConfigMap at its next spawn;
  let the session sleep or start a new one, then check the Pod's `imageID`.
- Where things show: hook and orchestrator output in the orchestrator Deployment's
  logs, lifecycle decisions and reconcile errors in the session controller's logs,
  the runner's own log in the Sandbox Pod, intent in the `shock.invalid/*`
  annotations on the Sandbox, and the orchestrator's `/healthz` JSON via the API
  server proxy (`kubectl get --raw /api/v1/namespaces/<ns>/pods/<pod>:8080/proxy/healthz`).
  Inspect a running runner with `kubectl exec`; treat account emails, work-order
  contents and anything under `/var/run/claude` as secrets when reading output.
- Egress policy needs Cilium: on kind, delete the `kindnet` DaemonSet, install the
  Cilium chart with `ipam.mode=kubernetes` and one operator replica, then restart
  every Deployment so Pods become Cilium endpoints. Secret injection (spec section 9)
  also needs the SDS policy-secret defaults (`tls.secretSync.enabled=true`) and,
  with Hubble, `hubble.redact` for the injected headers. Verify with a throwaway pod
  carrying the runner selector labels (`app.kubernetes.io/name=runner`,
  `app.kubernetes.io/instance=<release>`) and `curl`; Hubble (`hubble observe -n
  <ns> --verdict DROPPED` inside the agent) shows drops. `cilium-dbg fqdn cache list`
  shows what resolved. Keep `network.allowedFQDNs` disjoint from Anthropic's list;
  `make trusted-domains` refreshes the latter.
- Monitoring needs Prometheus Operator: on kind, kube-prometheus-stack with
  `podMonitorSelectorNilUsesHelmValues=false` and `ruleSelectorNilUsesHelmValues=false`
  picks up the chart's PodMonitor and rules. Query through the API server proxy
  of the Prometheus Service (`.../services/<svc>:9090/proxy/api/v1/targets`).
- Facts come from sources, not memory: upstream code at the pinned tag, the
  Anthropic docs pages (fetch `<page>.md`), release APIs for versions. Record what
  a live run corrected in the spec's section 13 with the date.
- Naming or Secret-shape changes make existing Sandboxes stale: the hook creates a
  fresh Sandbox and PVC for the session on its next order and GC reaps the old one.
  Say so in the commit and the spec.

## Validation and delivery

- For documentation changes, check consistency, relative links, and whitespace.
- Run the checks appropriate to the change: `make fmt vet lint test` for Go;
  `make helm-lint chart-golden` for the chart; `make envtest` for API concurrency;
  `make e2e-kind` for upstream lifecycle and owner-reference conformance. Use the
  spec's acceptance matrix.
- Do not substitute fake-client tests for API concurrency or garbage-collection tests.
- Keep README instructions accurate as implementation lands. State which checks ran
  and which could not run; do not imply unimplemented behavior was tested.
- Preserve unrelated user changes. Stage only intended files, and commit or push only
  when requested. Never amend a commit without authorization.
- Check exit codes directly. A test or lint step piped into `tail` or `grep` reports
  the filter's status, not its own; do not let a green-looking chain commit a red tree.
- Scripted edits: assert each replacement and apply them independently, so one
  mismatch cannot silently skip the rest or truncate a file; re-read the result.
- Comments state what is present and why the reader needs it, in a line or two.
  Rationale and history belong in the spec, README or commit message.
- Versioning: the author picks the bump from the change — a feature or behavior
  change is a minor, a fix, docs or a dependency bump a patch; both kinds in one
  release make it a minor. The automated Claude Code bump cuts a patch and
  refuses to release while `main` carries commits the newest tag does not cover,
  so unreleased work blocks it.
- When reusing the kind cluster, `make e2e-setup` and the e2e install step already
  clear leftover Sandboxes and restart the controller; still confirm the running
  controller's `imageID` matches the image just loaded when a run fails oddly.
