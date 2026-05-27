# AGENTS.md

## After every code change

Always run the following checks after modifying Go files:

```sh
make fmt-check
make vet
```

Fix any issues before committing.

## When making changes to the API

Always document them into docs/API.md

## Keep documentation up-to-date

Remember to update any relevant documentation in the docs/ folder if any of the changes affect them
