# Callbell CLI

Callbell CLI is a single command-line entry point to the knowledge and service backends you already run.
It gives people and automated agents the same predictable interface: one binary, named connections, and
output that is safe to pipe into other tools.

The binary is called `callbell`.

## Goal

Working across self-hosted tools usually means one ad-hoc script per system. Callbell CLI replaces that with
one client that discovers what a configured connection can do and exposes those capabilities through a
stable command surface, a stable output contract, and stable exit codes.

## MVP boundary

The first milestone deliberately stays small. It covers:

- a thin command layer with global options for configuration, connection selection, agent mode, and output
  format,
- configuration with multiple named connections and a secure local store for credentials,
- a capability registry so commands are resolved from what a connection actually offers,
- a documented contract for stdout, stderr, errors, and exit codes,
- read-only access to BookStack as the first provider,
- a terminal UI for editing configuration and testing a connection.

Not part of the MVP: write access to any provider, additional providers, packaging and release automation,
and any plugin or extension mechanism.

## Install

There is no prebuilt release yet, so the supported way to get the binary is to build it from source.
Requires Go 1.24 or newer.

```sh
git clone https://github.com/castrowithcee/callbell-cli.git
cd callbell-cli
go build -o callbell .
```

The `-o callbell` is not cosmetic. `go build .` without it names the output after the module, so it would
leave you with a file called `callbell-cli`, while the command this documentation uses is `callbell`.

Verify the build, then move the file into a directory on your `PATH` so you can call it from anywhere:

```sh
./callbell --version
./callbell --help
```

From there on, every example is a plain `callbell` call:

```sh
callbell --help
callbell capabilities
```

## Configuration

Callbell CLI reads one YAML file describing services, credentials, and the connections that bind them.
The file never holds a secret: a credential either names environment variables or points at the credential
store of your platform, which `callbell credential set` fills. A stored credential is resolved through one
cascade, environment variable before credential store before an explicitly enabled plaintext fallback, and
callbell always reports which of them delivered. A credential that names its environment variables is
resolved from those variables alone, so a CI run cannot silently pick up a local identity instead. See
[docs/configuration.md](docs/configuration.md) and the annotated
[examples/config.yaml](examples/config.yaml).

## Providers

BookStack is the first provider, read-only. See [docs/bookstack.md](docs/bookstack.md) for setup, least
privilege, and the two capabilities it offers.

## Terminal editor

`callbell tui` edits services, credentials, connections, and domain defaults through the same core and the
same validating, atomic store the CLI uses, and tests a selected connection with `t`. A keyring credential
is set up there completely: `s` on a secret role takes the value in a masked field and hands it to the
credential store, `p` writes it to the explicit plaintext fallback, `x` removes it. Nothing is ever
displayed back; every role shows which source delivers it, and the configuration file never holds a
secret.

## Output

Results are available as an aligned table, as lossless JSON, or as a compact machine format that `--agent`
selects automatically. Field order, exit codes, and error codes are a stable contract described in
[docs/output.md](docs/output.md).

## Agent access

Agents can use the one-request JSON commands `callbell search`, `callbell describe`, and `callbell invoke`,
or start `callbell mcp` as an MCP stdio subprocess. The MCP surface exposes the same three operations as
the fixed tools `callbell.search`, `callbell.describe`, and `callbell.invoke`; adding providers or provider
operations does not add tools. Both transports call the same application core. See
[docs/output.md](docs/output.md) for the stream, error, and cancellation contract.

## Development

Working on the code itself needs no installed binary:

```sh
go build ./...
go test ./...
go run . --help
```

`go run .` is a development shortcut only. Examples aimed at users always call the built `callbell` binary.

## License

MIT. See [LICENSE](LICENSE).
