---
description: >
  Telegram Bot API setup, confirmed plain-text sending, fixed connection targets, audit, and failures.
type: knowledge
edit: shared
created: 2026-08-20
updated: 2026-08-20
---

# Telegram

Callbell CLI exposes one Telegram mutation: `telegram.messages.send`. It sends one plain-text message with
the Bot API [`sendMessage`](https://core.telegram.org/bots/api#sendmessage) method. The provider also keeps
the read-only [`getMe`](https://core.telegram.org/bots/api#getme) connection test used by the TUI.

## Setup

A Telegram service names the HTTPS Bot API root, a credential supplies the `bot-token`, and each connection
binds that bot to one fixed target:

```yaml
version: 1
services:
  telegram-main:
    provider: telegram
    base_url: https://api.telegram.org
credentials:
  notifier:
    type: keyring
connections:
  alerts:
    service: telegram-main
    credential: notifier
    target: "-1001111111111"
defaults: {}
```

Store the BotFather token without putting it in the configuration:

```sh
printf %s "$BOT_TOKEN" | callbell credential set notifier bot-token
```

The target is a numeric chat ID or an `@channel` username. It exists only in the connection and cannot be
overridden by operation arguments. Use separate connections for separate targets, even when they share a
service or bot credential.

## Send one message

Send requests require both the connection and confirmation in the same JSON object:

```sh
printf '%s\n' '{"operation":"telegram.messages.send","connection":"alerts","arguments":{"text":"Deployment finished"},"confirm":true}' | callbell invoke
```

The text must contain 1 through 4096 characters. Markup modes, arbitrary Bot API parameters, free target
IDs, attachments, replies, edits, deletes, forwarding and bulk sends are not supported. A successful
result contains only `message_id` and Telegram's Unix `date` value.

Without `confirm: true`, without `connection`, or with invalid arguments, Callbell resolves no token,
writes no send-attempt audit event and performs no provider request. Defaults and the single matching
connection fallback do not apply to this mutation.

## Safety and audit

The adapter performs exactly one HTTPS `sendMessage` POST and follows no redirect. It never retries after a
timeout, connection failure or otherwise unclear result because a message may already have been accepted.
Provider responses are size-limited and validated before their two safe metadata fields reach stdout.

Every confirmed dispatch writes one JSON audit event to stderr. It contains a fresh request ID, operation,
connection, confirmation, outcome and timestamp. It never contains the text, target, bot token, request
headers or provider body. The requested result remains the only stdout payload.

Errors use the common output codes. Telegram distinguishes rejected credentials (`auth`), missing provider
permission (`permission`), throttling (`rate-limited`), request deadlines (`timeout`), other reachability
or TLS failures, invalid responses and general provider errors. Provider descriptions and response bodies
are never copied into diagnostics.
