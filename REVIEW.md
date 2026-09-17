# Reviewing pull requests

Most pull requests here are opened by a bot. This is what each kind expects of
a reviewer, and what merging one does to the release train.

## Dependabot

Dependabot opens at most three pull requests a month, one per group, configured
in [.github/dependabot.yml](.github/dependabot.yml).

| Group | Contents | Handling |
| --- | --- | --- |
| `actions` | every `uses:` in `.github/workflows` | Merge on green. Read the release notes of anything crossing a major. |
| `go` | Go modules outside the kubernetes set | Merge on green. |
| `kubernetes` | `k8s.io/*`, `sigs.k8s.io/agent-sandbox`, `sigs.k8s.io/controller-runtime` | A chore. See below. |

Actions are pinned by commit SHA with the version in a trailing comment;
Dependabot moves both together, so a diff that changes only the SHA or only the
comment is wrong.

The `kubernetes` group is not a merge-on-green, because Dependabot edits
`go.mod` and nothing else while two pins are derived from it, and neither of
them turns CI red when stale. Before merging, in the same pull request:

- re-derive `kubeVersion` in [charts/shock/Chart.yaml](charts/shock/Chart.yaml):
  two minors below the `k8s.io/*` version the new agent-sandbox release pins;
- move the `agent_sandbox` entry of the e2e matrix in
  [.github/workflows/ci.yaml](.github/workflows/ci.yaml), which names its own
  version independently of `go.mod`;
- run `make e2e-kind AGENT_SANDBOX_VERSION=vX.Y.Z`; a conformance failure blocks
  the bump;
- record the versions and evidence in section 13 of
  [plans/shock-spec.md](plans/shock-spec.md).

Dependabot does not touch the Go toolchain (`go.mod`'s `go` directive and
`ARG GO_VERSION` in the orchestrator image — the `go` job fails when those two
disagree), the Claude Code pin, or the image base tags.

## Merging blocks the next Claude release

Every merged pull request puts a commit on `main` that the newest `vX.Y.Z` tag
does not cover, and `release-claude.yaml` refuses to run in that state — a patch
version means a Claude Code bump and nothing else. So the morning after you
merge anything, including a Dependabot pull request, the daily run fails with
`main has N commit(s) since vX.Y.Z` until the work is released as
`vX.(Y+1).0`.

That is the intended behavior, not a fault: it keeps an unrelated change out of
a patch release. Cut the minor when you merge, rather than discovering it from a
red run. Both ecosystems are monthly so this happens in at most two windows a
month. See [CONTRIBUTING.md](CONTRIBUTING.md#releasing).
