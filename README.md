# Callbell CLI

Callbell CLI is a provider-based operations broker for the services you already use. It gives people and
automated agents one consistent, inspectable interface: configure connections once, discover
provider-qualified tools, and invoke the same operations from the terminal or over MCP.

The executable is named `callbell`. The current build includes BookStack, Telegram, Lexware, Twenty CRM,
SeaTable, and Nextcloud providers.

## Install

Tagged releases provide prefix-ready archives for these platforms:

| Platform | Architectures | Archive |
| --- | --- | --- |
| Linux | amd64, arm64 | `.tar.gz` |
| macOS | amd64, arm64 | `.tar.gz` |
| Windows | amd64 | `.zip` |

Download only from [GitHub Releases](https://github.com/castrowithcee/callbell-cli/releases), and verify the
archive against the published `checksums.txt`. Release binaries are not currently code-signed or notarized.

### Linux and macOS

The following installs the current release into the user-owned prefix `~/.local`. Set `system` to `linux`
or `darwin` and `architecture` to `amd64` or `arm64` for your machine. Replace `version` when a newer release
is available.

```sh
version=v0.6.0
system=linux
architecture=amd64
archive="callbell_${version}_${system}_${architecture}.tar.gz"
release="https://github.com/castrowithcee/callbell-cli/releases/download/${version}"

curl --fail --location --remote-name "${release}/${archive}"
curl --fail --location --remote-name "${release}/checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
	grep " ${archive}$" checksums.txt | sha256sum --check -
else
	grep " ${archive}$" checksums.txt | shasum -a 256 --check
fi

mkdir -p "$HOME/.local"
tar -xzf "$archive" -C "$HOME/.local"
```

Make sure `~/.local/bin` is in `PATH`, then verify the installation:

```sh
callbell --version
callbell --help
```

The archive also installs command manpages below `~/.local/share/man/man1`.

### Windows

In PowerShell, download and verify the Windows archive before extracting it into a user-owned prefix:

```powershell
$Version = "v0.6.0"
$Archive = "callbell_${Version}_windows_amd64.zip"
$Release = "https://github.com/castrowithcee/callbell-cli/releases/download/$Version"

Invoke-WebRequest "$Release/$Archive" -OutFile $Archive
Invoke-WebRequest "$Release/checksums.txt" -OutFile checksums.txt

$Expected = ((Get-Content checksums.txt | Where-Object { $_ -match [regex]::Escape($Archive) }) -split "\s+")[0]
$Actual = (Get-FileHash $Archive -Algorithm SHA256).Hash.ToLowerInvariant()
if ($Actual -ne $Expected) { throw "checksum verification failed for $Archive" }

$Prefix = Join-Path $env:LOCALAPPDATA "Programs\Callbell"
Expand-Archive $Archive -DestinationPath $Prefix
& "$Prefix\bin\callbell.exe" --version
```

Add `%LOCALAPPDATA%\Programs\Callbell\bin` to your user `Path` to invoke the executable as `callbell`.

## Quick start

Open the terminal editor to create services, credentials, and connections:

```sh
callbell tui
```

Validate the saved configuration and see whether every required secret can be resolved:

```sh
callbell config validate --secrets
```

Then discover and invoke the tools available in your installation:

```sh
callbell providers
callbell tools bookstack
callbell tool bookstack.pages.list
callbell invoke bookstack.pages.list --connection wiki --arg limit=10
```

`providers`, `tools`, and `tool` write [TOON 4.1](https://github.com/toon-format/spec/blob/v4.1.1/SPEC.md)
to stdout. Use `--output json` for the equivalent JSON representation. `invoke` accepts repeated
`--arg name=value` arguments, typed by the tool's input schema, or one JSON object on stdin for nested
input. Its result is JSON.

## How Callbell is organized

Callbell keeps provider details behind one tool taxonomy. A tool ID such as `bookstack.pages.list` consists
of a provider namespace and an operation:

- `callbell providers` lists the namespaces compiled into the installed release.
- `callbell tools <namespace>` lists the tools of one namespace. Use `--query <text>` for targeted search.
- `callbell tool <id>` prints the complete contract, including schemas, risk, examples, and configured
  connections.
- `callbell invoke <id>` validates the input and runs the selected operation.

Discovery never contacts a provider or resolves a secret. A connection name is the stable routing value
used by `--connection`; its optional description helps people and agents choose the intended route.

## Configuration and credentials

The default configuration file is `~/.callbell/cli/config.yaml`. Resolution follows this order:

1. `--config <path>`
2. `CALLBELL_CONFIG`
3. `CALLBELL_CLI_HOME/config.yaml`
4. `~/.callbell/cli/config.yaml`

The configuration separates three concerns:

- A service names one provider and its API endpoint.
- A credential describes where that provider's required secrets come from.
- A connection binds one service to one credential and, when needed, a provider-specific target.

The configuration contains references, never secret values. Keyring credentials use the platform's system
credential store by default and may be overridden by derived environment variables. Credentials of type
`env` resolve only from the variables they name. An explicitly enabled plaintext fallback is stored as
`credentials.yaml` beside the resolved configuration and must remain readable by its owner only.

[`examples/config.yaml`](examples/config.yaml) documents every field and includes safe placeholder
connections for all bundled providers. Connection descriptions are published by discovery, so they must
never contain secrets or personal data.

## MCP

`callbell mcp` serves the same application core over stdio through three fixed broker tools:
`callbell.search`, `callbell.describe`, and `callbell.invoke`. Configure an MCP host to launch this command
with access to the same configuration and credential environment as the terminal installation.

```sh
callbell mcp
```

## Update

On Linux and macOS, a tagged release installed directly as `<prefix>/bin/callbell` can update itself to the
latest stable release. The updater verifies the selected archive against its published SHA-256 checksum
before replacing the executable and main manpage.

```sh
callbell update --check
callbell update
```

The executable must be a regular file named `callbell` inside a writable `bin` directory. Development
builds, symlink installations, and installations the current user cannot write are not updated in place.
Windows can check for a newer release with `callbell update --check`, but in-place replacement of a running
Windows executable is not supported yet; download, verify, and extract the newer archive manually.

## Uninstall

Removing the program does not implicitly remove configuration or credentials. Before deleting the
executable, remove every stored keyring credential role you no longer want to keep:

```sh
callbell credential delete <credential> <role>
```

This command removes the entry from the system credential store and from the optional plaintext fallback.
It does not modify environment variables.

For the default Linux or macOS installation, remove the installed program files:

```sh
rm "$HOME/.local/bin/callbell"
rm "$HOME/.local/share/man/man1"/callbell*.1
rm -r "$HOME/.local/share/doc/callbell"
```

On Windows, remove `%LOCALAPPDATA%\Programs\Callbell` and its `bin` entry from your user `Path`.

To remove local configuration as well, delete the resolved `config.yaml` and, if it exists, the adjacent
`credentials.yaml`. Check `--config`, `CALLBELL_CONFIG`, and `CALLBELL_CLI_HOME` first if you did not use the
default path. Environment variables and any credential-store entries not removed before uninstalling
remain under the control of the shell, CI system, or platform credential manager that owns them.

## Development

Development requires Go 1.24 or newer.

```sh
go build ./...
go test ./...
go vet ./...
go run . --help
```

Command definitions stay thin adapters over the application core. Tests assert stdout, stderr, and exit
codes; provider tests use local HTTP servers and never require live credentials or services.

## Help

Use `callbell --help`, `callbell <command> --help`, or the installed `man callbell` pages for the command
reference. Report reproducible bugs through [GitHub Issues](https://github.com/castrowithcee/callbell-cli/issues)
without including configuration files, credentials, endpoints, or other private data.

## License

MIT. See [LICENSE](LICENSE).
