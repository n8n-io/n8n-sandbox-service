# AGENTS.md

## After every code change

Always run the following checks after modifying Go files:

```sh
make fmt-check
make vet
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

Always document them into docs/API.md

## Keep documentation up-to-date

Remember to update any relevant documentation in the docs/ folder if any of the changes affect them

## Firecracker runner

All Firecracker runner related files should contain `.ee` or be in a directory that contains `.ee` in the name, so the enterprise license applies.
