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
See [docs/configuration.md](docs/configuration.md) and the annotated
[examples/config.yaml](examples/config.yaml).

## Providers

BookStack is the first provider, read-only. See [docs/bookstack.md](docs/bookstack.md) for setup, least
privilege, and the two capabilities it offers.

## Terminal editor

`callbell tui` edits services, credentials, connections, and domain defaults through the same core and the
same validating, atomic store the CLI uses, and tests a selected connection with `t`. It never asks for or
displays a secret: a credential names environment variables, and the editor shows only whether a named
variable is set.

## Output

Results are available as an aligned table, as lossless JSON, or as a compact machine format that `--agent`
selects automatically. Field order, exit codes, and error codes are a stable contract described in
[docs/output.md](docs/output.md).

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
