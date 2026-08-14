# Callbell CLI

## Project

Callbell CLI is a Go command-line client that exposes self-hosted knowledge and service backends through one
binary named `callbell`. Module path: `github.com/castrowithcee/callbell-cli`.

The repository is self-contained. Everything needed to build, test, and understand the project lives here.
Do not introduce paths, tooling, or runtime dependencies that live outside this repository.

## Design principles

- Prefer the simplest solution that fully works. No speculative architecture or premature abstraction.
- Reuse the standard library before adding a dependency. Every dependency needs a concrete reason.
- Command definitions stay thin adapters over the application core; business logic lives outside them.
- Public surface — command names, flag names, output format, and exit codes — is a contract. Changing it is
  a deliberate decision, not a side effect of a refactor.

## Output and exit codes

- `stdout` carries requested payload data only.
- `stderr` carries diagnostics, progress, and errors.
- Exit code `0` on success, `2` on usage or validation errors, `1` on runtime errors.
- Agent mode produces no color, no spinners, and no success prose.

## Testing

Cover behavior with table-driven tests. Commands take injectable streams and never terminate the process
directly, so stdout, stderr, and exit codes stay assertable in tests.

Run before proposing a change:

```sh
go build ./...
go test ./...
go vet ./...
```

## Commits

Use English Conventional Commits that describe the code change only.

```
feat(config): support multiple named connections
fix(output): keep diagnostics off stdout
```

## Security

Never commit credentials, tokens, or personal data. Configuration examples use placeholders. Provider
credentials belong in the local credential store, never in the repository or in test fixtures.
