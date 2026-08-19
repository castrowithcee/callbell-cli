---
description: >
  Public stream, format, escaping, projection, limit, and error-output contract for Callbell CLI.
type: knowledge
edit: shared
created: 2026-08-14
updated: 2026-08-19
---

# Output contract

Callbell CLI writes requested data to stdout and everything else to stderr. Output is deterministic: the
same command over the same data produces byte-identical bytes.

## Streams and exit codes

| Stream | Carries |
| --- | --- |
| stdout | requested payload data only, plus `--help` and `--version` |
| stderr | diagnostics and errors |

| Exit code | Meaning |
| --- | --- |
| `0` | success |
| `2` | usage or validation error |
| `1` | runtime error |

## Formats

`--output table` (default), `--output json`, or `--output compact`.

`--agent` selects `compact` and suppresses color and prose. An explicit `--output` always wins over
`--agent`.

Every command returns either a **collection** of records that share one column order, or a single
**object**. Field order is stable and comes from the command, not from Go's map iteration.

### table

The human format. Columns are aligned, the header is uppercase. Line breaks, carriage returns, and tabs
inside a value are shown escaped so a record stays on one line.

```
NAME                  RISK  DESCRIPTION
knowledge.pages.get   read  Read one page
knowledge.pages.list  read  List pages
```

For an object, each line is a field name and its value. Null and empty values are omitted.

### json

Lossless and typed. Numbers stay numbers, booleans stay booleans, and a value a record does not have is an
explicit `null`. Objects keep every field, including null and empty ones.

```json
[{"name":"knowledge.pages.get","risk":"read","description":"Read one page"}]
```

### compact

The agent format. Smaller than JSON for tabular data and unambiguous to split.

A **collection** is a header line followed by one line per record, fields separated by `|`:

```
name|risk|description
knowledge.pages.get|read|Read one page
knowledge.pages.list|read|List pages
```

A missing or empty value keeps its column as an empty field, so every line has the same number of fields.

An **object** is one `key=value` per line. Null and empty values are omitted:

```
name=knowledge.pages.get
risk=read
description=Read one page
```

#### Escaping

Values and keys are escaped so that a separator, a backslash, and a real line break never look alike:

| Character | Escaped as |
| --- | --- |
| `\` | `\\` |
| `|` | `\|` |
| newline | `\n` |
| carriage return | `\r` |
| `=` (objects only) | `\=` |

Escaping is a single pass, so an escaped backslash is never escaped again. To decode, scan left to right and
treat `\` as introducing exactly one escaped character.

## Projection and limits

`--fields a,b` restricts the output to those fields and returns them in that order. Projection is validated
centrally, so the same names work for every command and format.

- An unknown name is a usage error that lists the available fields.
- Repeating a name is a usage error, because it would produce a duplicate key in JSON.
- Omitting the flag, or passing `--fields ""`, keeps every field.

`--limit n` caps a collection at `n` records. The default is `50`. `--limit 0` and any negative value remove
the cap. Objects are unaffected.

## Errors

The first line on stderr carries a provider-independent code:

```
callbell: <code>: <message>
```

A runtime error (exit `1`) is that line and nothing else. A usage error (exit `2`) is followed by the usage
text of the command that reported the error, so stderr is longer than one line. For example, an error from
`callbell knowledge pages list` shows that command's usage rather than the root command's usage. Read the
first line and branch on the code rather than on the message text.

| Code | Meaning |
| --- | --- |
| `usage` | wrong invocation, unknown flag, unknown field |
| `config-missing` | no configuration file at the resolved path |
| `config-invalid` | the configuration file does not satisfy the schema, or a file beside it is not usable as it stands, for example a credential fallback others can read |
| `connection-selection` | no connection given and no usable default |
| `unknown-connection` | the named connection is not configured |
| `unsupported-capability` | the capability is not offered |
| `missing-secret` | a credential yields no secret: the message names the configuration key to fix |
| `unreachable` | the provider host did not answer |
| `tls` | the TLS connection to the provider could not be established |
| `auth` | the provider rejected the credential |
| `rate-limited` | the provider refused further requests for now |
| `provider-error` | the provider answered with something unusable |
| `runtime` | anything else that failed while running |

Known secret values are removed from string payload values before successful output is encoded, and from
every error message before it is shown. This also covers a provider that returns the credential it received.
Redaction happens before JSON or compact escaping, so those formats stay valid and cannot hide a secret
from the redactor.
