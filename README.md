# Callbell CLI

Callbell CLI is a Go command-line client for self-hosted knowledge and service backends. The executable is
named `callbell`.

## Usage

People configure Callbell with `callbell tui`. Agents use one tool taxonomy, where a tool is one
provider-qualified operation such as `bookstack.pages.list`:

```sh
callbell tools                     # the local catalog
callbell tools bookstack           # one namespace
callbell tools --query pages       # a text filter over the same catalog
callbell tool bookstack.pages.list # one complete contract
echo '{"limit":10}' | callbell invoke bookstack.pages.list --connection wiki
```

`tools` and `tool` write TOON 4.1 (https://github.com/toon-format/spec/blob/v4.1.1/SPEC.md) to stdout;
`--output json` returns the same data as JSON. `invoke` reads its arguments as one JSON object from stdin
and writes a JSON result. `callbell mcp` serves the same core over stdio as the three fixed broker tools
`callbell.search`, `callbell.describe`, and `callbell.invoke`.

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
