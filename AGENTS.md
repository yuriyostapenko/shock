# Working on SHOCK

## Start here

- Read `README.md` and all of `plans/shock-spec.md` before implementation.
- The first implementation is in place: `charts/shock`, `cmd/shock`, `internal/*`,
  `images/orchestrator`, `test/*` and the Makefile. Run `make all` before and after a
  change; lifecycle changes also need `make envtest` and `make e2e-kind`.
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
