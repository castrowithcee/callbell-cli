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
- A **credential** is only the source of secrets. It says where they come from; it never holds a value.
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
| `type` | yes | `keyring` or `env`. |
| `values` | only for `env` | Map of secret role to environment variable **name**. Must be absent for `keyring`. |

Secret roles are defined by the provider. BookStack requires `token-id` and `token-secret`.

**`type: keyring`** keeps the secrets in the credential store of your platform. The entry names nothing
else, so this file cannot hold a secret even by accident:

```yaml
credentials:
  wiki-reader:
    type: keyring
```

```sh
printf %s "$TOKEN_ID" | callbell credential set wiki-reader token-id
printf %s "$TOKEN_SECRET" | callbell credential set wiki-reader token-secret
```

**`type: env`** names the variables that carry the secrets. It is the way for CI, for containers, and for
any headless machine:

```yaml
credentials:
  wiki-auditor:
    type: env
    values:
      token-id: CALLBELL_WIKI_AUDITOR_TOKEN_ID
      token-secret: CALLBELL_WIKI_AUDITOR_TOKEN_SECRET
```

A secret value is never written to the configuration file and never appears in output or in an error
message. A message about a credential names the configuration key, never the text you put there: that text
may itself be a pasted token, and no rule can tell a pasted BookStack token apart from a legal variable
name.

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

## Where a secret comes from

A `keyring` credential is resolved through one cascade. The first stage that delivers wins:

1. **the environment variable**
2. **the system credential store**
3. **the plaintext fallback file**, and only when it was switched on explicitly

The order is the usual one: the more explicit and the more short-lived source wins, the way `gh`, the AWS
CLI and `kubectl` resolve theirs. Because overriding is allowed, callbell always tells you which stage
delivered, so a forgotten variable cannot shadow the credential store in silence.

**An `env` credential stops after stage 1.** It names its variable, and that says everything about where
its secret comes from. If the variable is not set, the command fails; a credential store entry or a
plaintext file on the same machine never stands in for it. That is what makes `type: env` the dependable
choice for CI and headless runs: a forgotten variable stays a visible failure instead of quietly
authenticating with whatever identity happens to lie next to the configuration.

### 1. The environment variable

A credential of `type: env` names its variable itself. A credential of `type: keyring` has a derived name:

```
CALLBELL_<CREDENTIAL>_<ROLE>
```

upper-cased, with every character that is not a letter or a digit replaced by an underscore. The
credential `wiki-reader` and the role `token-id` therefore read `CALLBELL_WIKI_READER_TOKEN_ID`. Setting
that variable overrides whatever is in the store, which is what makes a stored credential usable in a
container without changing the configuration.

### 2. The system credential store

Secret Service on Linux, the Keychain on macOS, the Credential Manager on Windows. Entries live under the
service `callbell-cli` with the account `<credential>/<role>`, so you can see and remove them with the
tools of your platform.

```sh
printf %s "$TOKEN" | callbell credential set wiki-reader token-id
callbell credential delete wiki-reader token-id
```

The secret is read from standard input, so it never reaches the command line or the shell history. No
command ever shows a stored secret back.

`delete` clears the credential store and the plaintext fallback together. It reports success only when no
place kept an entry back: if one of them could not be cleared, the command says which one still holds the
secret, what was already removed, and how to fix it. A half-done delete is never reported as done.

Every call into the credential store runs under a 30 second deadline, long enough to answer an unlock
dialog and short enough that a stuck keyring daemon cannot freeze a run. A deadline that passes counts as
a store that could not be reached: the cascade moves on and the report says `credential store (timed
out)`.

Set `CALLBELL_CREDENTIAL_STORE=none` to skip the store entirely. A CI job or a container that resolves
from the environment anyway wants this: no D-Bus call, no unlock prompt. The only other accepted value is
`auto`, the default; anything else is rejected with a usage error rather than quietly meaning `auto`.

### 3. The plaintext fallback

A machine without a working credential store is not a dead end, but the way out is explicit:

```sh
printf %s "$TOKEN" | callbell credential set wiki-reader token-id --plaintext
```

This writes `credentials.yaml` next to `config.yaml`, with mode `0600` and `allow_plaintext: true` in it.
Both are required for the file to be read, so a leftover file cannot deliver a secret by accident, and
without `--plaintext` the file is never created at all: `callbell credential set` fails and names this
option instead of falling back quietly.

The mode is enforced, not just written. A fallback that anyone but its owner can read or write is refused,
the way `ssh` refuses a private key that is too open, and the message names the fix:

```
credentials.yaml holds secrets in clear text but its mode is 0644; it is not read until it is
private again: chmod 600 <path>
```

On Windows the mode is not checked: access there is governed by ACLs that a Unix mode cannot express, and
the Credential Manager is the store to use on that platform anyway.

### When nothing delivers

The message names the stages that were tried and the way out. It never repeats the text you configured,
never a stored value, and never a line of the fallback file:

```
callbell: missing-secret: credentials.wiki-reader, secret role token-id does not yield a secret;
checked: environment variable (not set), credential store (no entry), plaintext file (not enabled);
store it with 'callbell credential set wiki-reader token-id', or export CALLBELL_WIKI_READER_TOKEN_ID
```

A fallback others can read is one state with one fix, so reading, writing and deleting all report it as
`config-invalid` with the same `chmod` hint, rather than as three different problems.

## Validating

```sh
callbell config validate
callbell config validate --config ./config.yaml
callbell config validate --secrets
```

The command reports every problem it finds at once, on stderr, and exits with code `2`. A valid file
produces no output and exits with `0`.

`--secrets` additionally resolves the secrets of every connection and reports where each one comes from:

```
connection   credential    role          source                checked
wiki         wiki-reader   token-id      environment variable
wiki         wiki-reader   token-secret  credential store      environment variable (not set)
```

It prints where a secret comes from, never what it is. It is not the default because it may ask the
credential store to unlock, while plain validation only reads a file. A role that no stage delivers is
listed with the source `missing` and the stages that were tried.

The report always covers every connection, because a check that silently dropped rows would read as an
all-clear for the rows it never showed. `--limit` therefore does not apply to it, and a `--limit` you set
yourself is refused as a usage error rather than accepted and ignored.
