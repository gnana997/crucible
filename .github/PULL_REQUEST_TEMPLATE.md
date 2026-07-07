<!--
Thanks for contributing to crucible! Keep the PR focused on one logical change.
For non-trivial changes, please open an issue to discuss the approach first
(see CONTRIBUTING.md).
-->

## What & why

<!-- What does this change, and why? Link the issue if there is one (e.g. "Fixes #123"). -->

## How was it verified?

<!-- How did you test it? For runner/jailer/network changes, describe the end-to-end
     run (which smoke test, on what host), not just "it compiles". -->

## Checklist

- [ ] `make fmt vet test` is clean (`make lint` too, if you have golangci-lint)
- [ ] The change is focused — unrelated cleanup is in a separate PR
- [ ] Tests added or updated (or a note on why the behavior is hard to unit-test)
- [ ] Docs updated if behavior, flags, or the API changed
- [ ] I did **not** weaken an isolation boundary (jailer / networking / clone-safety) for convenience
