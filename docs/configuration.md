# Configuration

Callbell CLI reads one YAML file. A ready-to-edit example is [`examples/config.yaml`](../examples/config.yaml).

## Where the file comes from

The first of these that applies wins:

1. `--config <path>`
2. the `CALLBELL_CONFIG` environment variable, which names the file itself
3. the `CALLBELL_CLI_HOME` environment variable, which names a directory holding `config.yaml`
4. `~/.callbell/cli/config.yaml`, the default on every platform

Commands that need no configuration work without a file. A command that needs a connection reports a usage
error when the file is missing.

## The model

```
provider -> service -> connection -> credential
```

- A **service** is one technical API endpoint of one provider.
- A **credential** is only the source of secrets. It names environment variables; it never holds values.
- A **connection** binds exactly one service to exactly one credential and is what you select at the
  command line.

That separation is the point: several instances of one provider are several services, and several keys for
one instance are several credentials pointing at the same service. Neither requires duplicating the other.

## Schema

The file must declare `version: 1`. Unknown keys, duplicate keys, and references to entries that do not
exist are errors.

### Names

The names of services, credentials, connections, and default domains consist of letters, digits, `-`, `_`,
or `.`, and start and end with a letter or a digit. `wiki-primary`, `wiki-audit`, and `knowledge` are
valid; a name with a space or a slash is rejected. These names are what you pass to `--connection`, so they
stay free of quoting.

### `services`

| Key | Required | Meaning |
| --- | --- | --- |
| `provider` | yes | Provider type. Currently `bookstack`. |
| `base_url` | yes | Base URL of the instance. Scheme `https`, or `http` for a local test server. |
| `options` | no | Non-secret provider-specific options as string values. |

### `credentials`

| Key | Required | Meaning |
| --- | --- | --- |
| `type` | yes | Only `env` is supported. |
| `values` | yes | Map of secret role to environment variable **name**. |

Secret roles are defined by the provider. BookStack requires `token-id` and `token-secret`.

```yaml
credentials:
  wiki-reader:
    type: env
    values:
      token-id: CALLBELL_WIKI_READER_TOKEN_ID
      token-secret: CALLBELL_WIKI_READER_TOKEN_SECRET
```

Secret values live in your environment or in whatever secret manager exports them. They are never written
to the configuration file and never appear in output or error messages.

### `connections`

| Key | Required | Meaning |
| --- | --- | --- |
| `service` | yes | Name of a service in this file. |
| `credential` | yes | Name of a credential in this file. |
| `target` | no | Optional provider-specific scope inside the service. |

Several connections may reuse the same service, the same credential, or both.

### `defaults`

```yaml
defaults:
  connections:
    knowledge: wiki
```

`defaults.connections.<domain>` names the connection a domain uses when `--connection` is not given. A
domain can have at most one default; a repeated domain key is rejected when the file is read.

## Selecting a connection

1. `--connection <name>` wins. An unknown name is a usage error.
2. Otherwise the default for the command's domain applies.
3. If neither is available, the command reports a usage error naming the default it expects.

Selection is local. It reads the file, resolves names, and contacts nothing.

## Validating

```sh
callbell config validate
callbell config validate --config ./config.yaml
```

The command reports every problem it finds at once, on stderr, and exits with code `2`. A valid file
produces no output and exits with `0`.
