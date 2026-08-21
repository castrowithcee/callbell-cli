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

`tools` is the discovery index: one entry per tool, carrying its `id` and the number of configured
`connections` that can run it, so a tool without a route stays visible with `0`. Everything else about a
tool belongs to `tool <id>`, which prints the full contract and names each connection with its stable
invoke value and the optional one-line `description` its owner maintains.

`tools` and `tool` write TOON 4.1 (https://github.com/toon-format/spec/blob/v4.1.1/SPEC.md) to stdout;
`--output json` returns the same data as JSON. `invoke` reads its arguments as one JSON object from stdin
and writes a JSON result. `callbell mcp` serves the same core over stdio as the three fixed broker tools
`callbell.search`, `callbell.describe`, and `callbell.invoke`.

## Configuration

The configuration file follows the model provider -> service -> connection -> credential; `callbell tui`
edits it and `examples/config.yaml` documents every field. A connection accepts an optional
`description`: one line of at most 200 characters that says what the route is for. It is published by
discovery, it is never sent to a provider, and it never selects a route: the connection name stays the
only invoke value. Because it is published, it must never carry a secret or personal data.

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
