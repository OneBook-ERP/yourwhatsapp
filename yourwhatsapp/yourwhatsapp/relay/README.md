# yourwhatsapp relay

Self-hosted WhatsApp sending/receiving layer for the Frappe bench, written in Go on top of [whatsmeow](https://github.com/tulir/whatsmeow) (direct multi-device websocket, no headless browser). One static binary serves every paired number: one whatsmeow client per `WhatsApp Account`. It is an internal ops tool, not approved by or affiliated with Meta/WhatsApp, and not "ban-proof".

Design: `docs/superpowers/specs/2026-10-31-whatsmeow-relay-design.md`. Production setup: `DEPLOYMENT.md`.

## Build and run

Go is installed at `$HOME/.local/go/bin` on the dev container and is not on `PATH`.

```bash
export PATH=$HOME/.local/go/bin:$PATH
cd apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay
make build   # CGO_ENABLED=0 go build -o bin/relay ./cmd/relay
make test    # go test ./...
make run     # go run ./cmd/relay
```

```bash
RELAY_API_KEY=<key> FRAPPE_BASE_URL=http://127.0.0.1:8001 RELAY_PORT=8800 \
  RELAY_DATA_DIR=$PWD/data bin/relay
```

`bin/relay healthcheck` probes `http://127.0.0.1:$RELAY_PORT/health` and exits 0 or 1 (used by the Docker `HEALTHCHECK`).

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `RELAY_API_KEY` | empty | Optional. When set, the relay is in **env mode**: the key comes from env and must equal `WhatsApp Settings.relay_api_key`. When empty, the relay uses its stored claim or waits to be claimed. A stored claim is ignored while it is set (the relay logs a warning at startup). |
| `RELAY_BOOTSTRAP_TOKEN` | empty | Required in production (optional on a dev bench): `POST /claim` must carry it as `bootstrap_token`. Use `python3 -c "import secrets;print(secrets.token_urlsafe(32))"`; `/claim` is not throttled. |
| `FRAPPE_BASE_URL` | `http://127.0.0.1:8001` | Callback base URL, **env mode only** (a claimed relay uses the callback URL from its claim). Use `127.0.0.1`, since `.localhost` names do not resolve from the relay process. |
| `FRAPPE_SITE_NAME` | empty | Env mode only: sent as the `Host` header on callbacks (e.g. `crm.example.com`). A claimed relay uses the claim's `site_name`. |
| `RELAY_HOST` | `127.0.0.1` | Listen address. `0.0.0.0` in Docker. |
| `RELAY_PORT` | `8800` | Listen port (named `RELAY_PORT`, not `PORT`, which collides). |
| `RELAY_DATA_DIR` | `./data` | Holds `relay.db`. Back it up. |
| `SEND_DELAY_MIN` / `SEND_DELAY_MAX` | `4s` / `12s` | Per-account random delay between outgoing messages (Go durations). |
| `MEDIA_MAX_BYTES` | `15728640` (15 MiB) | Largest inbound attachment the relay downloads and forwards. Bigger media arrives as text with an `[attachment too large: <name or type>, <n> MB]` note. Keep it at or below Frappe's max file size. |

## Modes

| Mode | When | Behaviour |
|---|---|---|
| `env` | `RELAY_API_KEY` is set | Key, callback URL and Host come from env. `/claim` answers 409 `already_claimed`; `/claim/rotate` and `/claim/callback` answer 409 `env_mode`. Dev bench and back-compat. |
| `claimed` | no env key, `relay_config` row in `relay.db` | Key, callback URL and site name come from the claim. |
| `unclaimed` | no env key, no claim | Only `/health`, `POST /claim` and `GET /info` work; every other route answers 503 `{"error":"unclaimed"}`. No sessions are restored and the outbox worker is idle. A successful claim starts both without a restart. |

A data dir that already holds linked numbers (accounts, devices or outbox rows) but has no claim refuses to start unless `RELAY_API_KEY` or `RELAY_BOOTSTRAP_TOKEN` is set, so an exposed port can't be used to take over live numbers. With the token set, the claim must carry it. Fresh volumes are unaffected.

## Routes

Every route except `/health`, `POST /claim` and `GET /info` needs the `x-api-key` header. Responses are JSON.

| Method & path | Notes |
|---|---|
| `GET /health` | `{"ok":true}`. No auth. |
| `POST /claim` | Unclaimed only. Body `{api_key, callback_url, site_name, bootstrap_token?}` returns 200 `{"claimed":true}`. Errors: 409 `already_claimed`, 401 `bad_bootstrap_token`, 400 `bad_request` (key under 32 chars; callback URL not http(s), or carrying credentials, a query or a fragment; `site_name` empty, over 253 chars, or not made of `A-Z a-z 0-9 . - :`), 413 for a body over 64 KB. Once the claim is otherwise valid, the relay first POSTs `<callback_url>/api/method/yourwhatsapp.yourwhatsapp.api.ping` with the candidate key and `Host: <site_name>` (10 s timeout); a non-2xx or no answer is 400 `callback_unreachable` with `detail` = the HTTP status (`http 404`) or an error class (`timeout`, `dns error`, `connection error`), and nothing is stored. |
| `POST /claim/rotate` | Needs the **current** key. Body `{api_key}` (new, 32+ chars). The new key applies at once for auth and callbacks; the old key is still accepted for other routes for a 10-minute grace window, but cannot rotate or change the callback. Retrying a rotation the relay already applied (grace key plus the current key in the body) is a 200 no-op. 409 `env_mode`, 503 `unclaimed`, 401 bad key. |
| `PUT /claim/callback` | Needs the current key. Body `{callback_url, site_name}` (same rules and 64 KB cap as `/claim`). The new target is pinged with the current key first, as for `/claim`: 400 `callback_unreachable` keeps the old target. Also 409 `env_mode`, 400 `bad_request`, 401. The new target applies to callbacks already queued in the outbox too. |
| `GET /info` | No key: `{version, claimed, mode}`. With a valid key (current or grace) also `sessions {connected, total}` (live connected sessions / linked numbers), `outbox {pending, dead}`, `callback_url` (credentials redacted) and `site_name`. Never returns a key. |
| `GET /outbox/dead` | Up to 200 dead-lettered callbacks, oldest first. |
| `POST /outbox/dead/replay` | Moves every dead-lettered callback back to the end of the outbox, in original order, with attempts reset. Returns `{"replayed": n}`. WhatsApp Settings shows **Relay → Replay Dead Callbacks** when `/info` reports dead rows. |
| `POST /session/:id/start` | Optional `{"proxy": ...}` (`""` clears the stored proxy, absent/`null` keeps it). Creates the account if new; connects if paired, otherwise starts the QR flow. Idempotent: on a connected session it returns `connected` and pushes `Active` to Frappe again. |
| `POST /session/:id/pair-code` | `{"phone": "+91 98765 43210"}` returns `{"code": "ABCD-1234"}`. Session must be unpaired. |
| `GET /session/:id/status` | `{"status", "jid", "phone"}` |
| `POST /session/:id/pause` | Disconnect and do not restore on boot. |
| `POST /session/:id/resume` | Optional `{"proxy": ...}`. Returns `{"ok": true, "status": ...}`. |
| `POST /session/:id/logout` | Unlink the device from WhatsApp, delete its keys and clear the pause. |
| `POST /message/send` | `local_message_id` required. Asynchronous: **202** `{"state":"queued"}`; the result arrives later as a `message_status` callback. Errors: 400 `bad_request`, 422 `invalid_number`, 409 `session_unavailable`, 429 `temp_banned`; 200 `duplicate` for an already queued or sent id. Asynchronous failures carry `send_failed`, `session_unavailable`, `temp_banned`, `relay_restarted` (a restart lost the queued send; Frappe retries it on the same account), or `send_unconfirmed` (the send errored after the message went out, so it may have been delivered; it carries `message_id` and is not failed over). |

`version` is stamped at build time (`make build` uses `git describe`; `-ldflags "-X main.version=..."`) and is `dev` otherwise. The Dockerfile takes it as a build arg: `docker build --build-arg VERSION=<tag> ...` (default `dev`).

Status values: `not_started`, `qr_pending`, `pairing`, `connecting`, `connected`, `disconnected`, `logged_out`, `paused`, `temp_banned`.

Callbacks into Frappe (`yourwhatsapp.api`): `update_qr`, `inbound_message`, `message_status`, `account_event`. They go through a persistent SQLite outbox with retry and backoff, so a Frappe outage does not lose messages; permanently failed rows move to `outbox_dead`.

## Behaviour worth knowing

- Only 1:1 chats (`@c.us` / `@lid`) are relayed. Status updates, groups, broadcast lists and channels are dropped.
- Attachments are base64 in the request body (limit 25 MB); the relay never fetches URLs.
- A paired session whose connect fails (at boot, on start/resume, after a connect failure or ban expiry) is retried after 5, 10, 20, 40, 80 and 160 s, then every 300 s (±20 % jitter), until it connects, is paused or logged out, or the relay stops. A replaced stream (another client on the same keys) is never retried. Socket drops on a working connection are retried by whatsmeow itself.
- Frappe is told `Disconnected` (`update_qr`) only after a session has been down for 60 s straight, so short blips don't flap the account; `Active` follows when it reconnects. Frappe also polls `GET /session/<id>/status` every 5 minutes to catch a missed callback.
- Sessions are restored from `relay.db` on boot, so a restart does not need re-scanning. Sends that were still queued when the relay stopped are reported Failed with `relay_restarted` 30 s after it is listening again; Frappe retries them on the same account (no failover) once the sessions have reconnected.
- A proxy change (set or cleared in Frappe) applies when the relay next builds the account's client. `start` on a running session ignores it, so Pause, then Resume, a connected account to apply it.
- Never run two relays against the same data dir or the same linked number: WhatsApp replaces the stream and the two fight.
- `relay.db` holds the device keys. Anyone with a copy can act as the linked number: treat it and its backups like credentials, and never expose the relay port to the internet.
- The Frappe side of claiming (WhatsApp Settings → Relay → Connect / Rotate / Retry) serialises key changes with a MariaDB named lock (`GET_LOCK`), so the claim UI requires a MariaDB-backed site. Env-mode relays work on any database.
- Logs are JSON on stdout with `session_id` on per-account lines. Message bodies, media, QR codes and keys are never logged.
