# BookStack

Callbell CLI reads pages from [BookStack](https://www.bookstackapp.com/) over its REST API. Access is
read-only: the provider only ever sends `GET` requests.

## Setting up access

1. In BookStack, open your profile and create an API token. You receive a **token ID** and a **token
   secret**. The secret is shown once.
2. Describe the instance and the credential in your configuration. The file holds no secret:

   ```yaml
   version: 1
   services:
     wiki:
       provider: bookstack
       base_url: https://wiki.example.com
   credentials:
     wiki-reader:
       type: keyring
   connections:
     wiki:
       service: wiki
       credential: wiki-reader
   defaults:
     connections:
       knowledge: wiki
   ```

3. Hand both halves of the token to the credential store:

   ```sh
   printf %s "$TOKEN_ID" | callbell credential set wiki-reader token-id
   printf %s "$TOKEN_SECRET" | callbell credential set wiki-reader token-secret
   ```

   On CI, or on a machine without a credential store, use `type: env` instead and export the variables
   the credential names. Both paths and the plaintext fallback are described in
   [configuration.md](configuration.md#where-a-secret-comes-from).

4. Check the file: `callbell config validate`. It only reads the configuration; it contacts no instance
   and reads no secret values. `callbell config validate --secrets` additionally shows which source
   delivers each secret. To check that the instance actually answers, run a real read:

   ```sh
   callbell knowledge pages list --limit 1
   ```

   Or open `callbell tui`, go to Connections, and press `t` on the connection. The editor reports one of
   the classes listed under Failures and never shows the credential.

See [configuration.md](configuration.md) for the full schema, including several instances and several keys
per instance.

## Least privilege

**A BookStack API token inherits the permissions of the user it belongs to.** Callbell CLI only issues read
requests, but the token itself would allow more in other hands. Create a dedicated BookStack user with a
role limited to viewing the content you want to reach, and issue the token for that user.

## Capabilities

| Capability | Command |
| --- | --- |
| `knowledge.pages.list` | `callbell knowledge pages list` |
| `knowledge.pages.get` | `callbell knowledge pages get <id>` |

```sh
callbell knowledge pages list --limit 20
callbell knowledge pages list --connection wiki-audit --fields id,name --agent
callbell knowledge pages get 42 --output json
```

`--limit` and `--offset` are passed to the API. `--fields` selects and orders the returned fields; run
`callbell describe knowledge.pages.list` to see which fields exist.

`--limit 0` fetches every page, in requests of at most 500 records, and holds the whole result in memory
before writing it. On a large instance prefer a concrete `--limit` with `--offset`. Records are
deduplicated by page id, and listing stops as soon as a response brings nothing new, so an instance that
ignores `offset` cannot inflate the result.

## Trust boundaries

- **Page content is untrusted data.** `html` and `markdown` are passed through to the selected output
  format exactly as received. Callbell never renders, interprets, or stores them. Treat them as untrusted
  in whatever consumes the output.
- **The base URL is a trust boundary.** Use `https` for any real instance. TLS verification is never
  disabled.
- **Redirects stay on the configured origin.** A redirect to a different scheme or host is refused before
  the credential would travel to it.
- **Credentials never appear in output.** The token is removed from every message, including from an
  unexpected error a server returns.

## Failures

Errors follow the [output contract](output.md) and carry one of these codes:

| Code | Meaning |
| --- | --- |
| `auth` | the token was rejected, or it lacks permission |
| `unreachable` | the host did not answer |
| `tls` | the TLS connection could not be established |
| `rate-limited` | BookStack refused further requests for now |
| `provider-error` | the instance answered with something unusable |
| `missing-secret` | the credential names no variable for a role, or that variable is not set |
