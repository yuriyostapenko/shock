# Contributing to SHOCK

SHOCK is at the specification stage. The first implementation should follow
[plans/shock-spec.md](plans/shock-spec.md), beginning with its upstream verification
items. Architectural changes should include the corresponding spec update.

## Development workflow

1. Read the README, spec, and repository instructions in `AGENTS.md`.
2. Choose a bounded change and identify its acceptance criteria.
3. Implement it with the relevant documentation and validation.
4. Review the diff for accidental files, credentials, and unrelated changes.
5. Open a pull request describing the resulting behavior and the checks performed.

There is no build or test toolchain in the repository yet. Add executable build and
validation instructions with the implementation that introduces them. Planned tooling
includes Go, Helm, controller-runtime envtest, and kind; pin versions when adopted.

For Go changes, format code and run relevant tests and lint. Chart changes need schema
and rendered-manifest checks. Lifecycle changes need the spec's concurrency and upstream
conformance tests, not just fake-client unit tests. Documentation-only changes need a
consistency and link review rather than an application test run.

## Pull requests

Keep PRs focused. Explain the problem, resulting behavior, and validation. If an upstream
beta assumption changes, identify the pinned release and update the spec and conformance
tests together. Note incomplete verification explicitly.

Do not include credentials, work-order JWTs, real environment configuration, or account
email addresses in fixtures, logs, screenshots, or issues. Use synthetic test data.

## License

Contributions are made under the repository's [Apache License 2.0](LICENSE).
