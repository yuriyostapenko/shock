# Contributing to SHOCK

SHOCK implements [plans/shock-spec.md](plans/shock-spec.md). Architectural
changes include the corresponding spec update; the spec's verification record
(section 13) lists the upstream versions and evidence the implementation rests on.

## Development workflow

1. Read the README, the spec and `AGENTS.md`.
2. Choose a bounded change and identify its acceptance criteria in spec section 12.
3. Implement it with the relevant documentation and validation.
4. Review the diff for accidental files, credentials and unrelated changes.
5. Open a pull request describing the resulting behavior and the checks performed.

## Toolchain

Go 1.27, Helm 4.3, kind 0.33, Docker. Install the Go tools once:

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25
```

Dependency pins live in `go.mod` (agent-sandbox v1.0.2, controller-runtime
v0.25.1, k8s.io v0.37.0), in `Chart.yaml` (`kubeVersion`, derived from the
agent-sandbox pin) and in the e2e matrix in `.github/workflows/ci.yaml`. Bump
them together and re-derive `kubeVersion`.

## Checks by change type

| Change | Run |
| --- | --- |
| Go | `make fmt vet lint test` |
| Lifecycle (hook or session controller predicates, patches, GC) | the above plus `make envtest` and `make e2e-kind`; fake-client tests alone are insufficient |
| Chart | `make helm-lint helm-template chart-golden`; e2e if the runner template or RBAC changed |
| agent-sandbox bump | `make e2e-kind AGENT_SANDBOX_VERSION=vX.Y.Z`; conformance failures block the bump |
| Documentation | consistency, relative links, whitespace |

`make e2e-kind` creates a kind cluster named `shock-e2e`, installs agent-sandbox
from the upstream release manifest, builds the controller image and runs
`test/e2e`. `make e2e-teardown` removes the cluster. The same cluster serves as
a live test bed with the real images and an environment key;
`hack/live-values.example.yaml` and the "Full-cycle work on a live cluster"
section of `AGENTS.md` describe the steps.

## Releasing

A release is a SemVer tag `vX.Y.Z` (or `vX.Y.Z-rc.N`) on a commit that is on
`main`. Nothing version-shaped is committed: `Chart.yaml` stays at `0.0.0-dev`
and the release workflow injects the version. Pushing the tag runs
`.github/workflows/release.yaml`, which in one run:

1. builds and pushes `ghcr.io/<owner>/shock:X.Y.Z` (multi-arch, SBOM and
   BuildKit provenance attached), signs it keyless with cosign and, when the
   repository is public, records GitHub build provenance;
2. pins that image's digest into the chart defaults, packages the chart with
   `--version X.Y.Z --app-version X.Y.Z`, pushes it to
   `oci://ghcr.io/<owner>/charts/shock:X.Y.Z`, signs and attests it;
3. creates the GitHub Release with generated notes, both digests and the chart
   archive attached; a version with a pre-release suffix is marked pre-release.

To release: `git tag -a vX.Y.Z -m "vX.Y.Z" <commit-on-main> && git push origin vX.Y.Z`.
The workflow refuses a tag whose commit is not on `main`. Add a repository
ruleset for `refs/tags/v*` (creation, update, deletion restricted to admins)
once the repository is public or on a plan that offers rulesets for private
repositories. Pull requests build the image without publishing; pushes to
`main` publish nothing.

## Pull requests

Keep PRs focused. Explain the problem, resulting behavior and validation, and
state which checks could not run. If an upstream beta assumption changes,
identify the pinned release and update the spec and conformance tests together.

Do not include credentials, work-order JWTs, real environment configuration or
account email addresses in fixtures, logs, screenshots or issues. Use synthetic
test data (the e2e "work order" is a shell fragment, not a token).

## License

Contributions are made under the repository's [Apache License 2.0](LICENSE).
