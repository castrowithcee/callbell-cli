# Callbell CLI

Callbell CLI is a Go command-line client for self-hosted knowledge and service backends. The executable is
named `callbell`.

## Development

Requires Go 1.24 or newer.

```sh
go build ./...
go test ./...
go vet ./...
go run . --help
```

Command definitions stay thin adapters over the application core. Tests assert stdout, stderr, and exit
codes; provider tests use local HTTP servers and never require live credentials or services.

## License

MIT. See [LICENSE](LICENSE).
