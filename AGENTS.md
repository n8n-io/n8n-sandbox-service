# AGENTS.md

## After every code change

Always run the following checks after modifying Go files:

```sh
make fmt-check
make vet
make lint
make test
```

`make lint` needs golangci-lint v2.15.0, the version that CI pins. Install it with
`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0`.
It reports only issues in code that changed since `LINT_BASE`. Locally,
`LINT_BASE` is `origin/main` by default. CI sets its own base. Do not add
`//nolint` to silence a finding unless the code is correct as it is. Give the
reason in the comment, for example `//nolint:forbidigo // <reason>`.

After modifying anything under `sdk/`, run:

```sh
make sdk-test
```

After modifying shell scripts, run:

```sh
make shell-fmt
make shell-lint
```

These need `shfmt` and `shellcheck` (`brew install shfmt shellcheck`). Both are
enforced in CI, which pins shfmt v3.13.1 and shellcheck v0.11.0 — older
shellcheck releases report findings that v0.11.0 has since dropped, so match the
pinned version locally.

Fix any issues before committing.

## When making changes to the API

Always document them into docs/API.md.
Make sure for any changes we make the SDK is aligned.

## When changing the Helm chart

Any change under `charts/n8n-sandbox-service/` must bump `version` in `charts/n8n-sandbox-service/Chart.yaml`.
Render the chart afterwards to check the templates still produce what you expect:

```sh
bash charts/n8n-sandbox-service/render-tests.sh
```

## Keep documentation up-to-date

Remember to update any relevant documentation in the docs/ folder if any of the changes affect them.

Document what the code does today, never what is planned.
Keep documentation as short as possible.

`docs/examples/compose.*.yaml` are not exercised by CI: when changing the API ↔ runner env or TLS contract, check for inconsistencies.

## Security review before reporting done

After changing anything other than docs or tests, review the diff for security
regressions before you report the work done. Use a fresh context where the
tooling allows.

## Firecracker runner

All Firecracker runner related files should contain `.ee` or be in a directory that contains `.ee` in the name, so the enterprise license applies.

This applies to runner source only. Test files need no `.ee`: tag a
Firecracker-only e2e spec `FIRECRACKER_ONLY` instead.

## Conventions

### Comments

- Keep all comments short. Write them in Simplified Technical English.
- Document WHY only when the reason is not obvious. Do not document WHAT the
  code does, unless the code is unusual or unclear.
- When you use a bit shift (`<<`), add a comment that gives the value (for
  example, `4 << 20 // 4 MiB`).
- Document the purpose of each `.go` file. Test files and generated files
  (marked `DO NOT EDIT`) are exceptions.
- Document the purpose of a struct field when its name does not make it clear.

### Naming

- Give each lookup map a `byX` suffix that tells its key (for example,
  `sandboxesByID`). Sets (`map[K]struct{}`) and fields that mirror an external
  or wire format (for example, `Env`, `Labels`) are exceptions.
- Use descriptive names. Do not use abbreviations. Method receivers are an
  exception: they can use short names (for example, `func (s *Server)`).
