# yourwhatsapp relay

Self-hosted WhatsApp sending/receiving layer for the Frappe bench, written in Go on top of [whatsmeow](https://github.com/tulir/whatsmeow) (direct multi-device websocket, no headless browser). One static binary serves every paired number: one whatsmeow client per `WhatsApp Account`. It is an internal ops tool, not approved by or affiliated with Meta/WhatsApp, and not "ban-proof".

Design: `docs/superpowers/specs/2026-10-31-whatsmeow-relay-design.md`. Production setup: `DEPLOYMENT.md`.

The legacy Node/whatsapp-web.js files (`server.js`, `session_manager.js`, `frappe_client.js`, `package*.json`) in this directory are kept only until cutover and are deleted afterwards. Ignore them for the Go relay.

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
| `RELAY_API_KEY` | required | Shared secret; must equal `WhatsApp Settings.relay_api_key`. The relay refuses to start without it. |
| `FRAPPE_BASE_URL` | `http://127.0.0.1:8001` | Where callbacks are POSTed. Use `127.0.0.1`, since `.localhost` names do not resolve from the relay process. |
| `FRAPPE_SITE_NAME` | empty | Sent as the `Host` header on callbacks, for multi-site or container setups (e.g. `crm.example.com`). |
| `RELAY_HOST` | `127.0.0.1` | Listen address. `0.0.0.0` in Docker. |
| `RELAY_PORT` | `8800` | Listen port (named `RELAY_PORT`, not `PORT`, which collides). |
| `RELAY_DATA_DIR` | `./data` | Holds `relay.db`. Back it up. |
| `SEND_DELAY_MIN` / `SEND_DELAY_MAX` | `4s` / `12s` | Per-account random delay between outgoing messages (Go durations). |

## Routes

Every route except `/health` needs the `x-api-key` header. Responses are JSON.

| Method & path | Notes |
|---|---|
| `GET /health` | `{"ok":true}`. No auth. |
| `POST /session/:id/start` | Optional `{"proxy": ...}`. Creates the account if new; connects if paired, otherwise starts the QR flow. Idempotent. |
| `POST /session/:id/pair-code` | `{"phone": "+91 98765 43210"}` returns `{"code": "ABCD-1234"}`. Session must be unpaired. |
| `GET /session/:id/status` | `{"status", "jid", "phone"}` |
| `POST /session/:id/pause` | Disconnect and do not restore on boot. |
| `POST /session/:id/resume` | Optional `{"proxy": ...}`. |
| `POST /session/:id/logout` | Unlink the device from WhatsApp and delete its keys. |
| `POST /message/send` | `local_message_id` required. Asynchronous: **202** `{"state":"queued"}`; the result arrives later as a `message_status` callback. Errors: 400 `bad_request`, 422 `invalid_number`, 409 `session_unavailable`, 429 `temp_banned`; 200 `duplicate` for an already queued or sent id. |

Status values: `not_started`, `qr_pending`, `pairing`, `connecting`, `connected`, `disconnected`, `logged_out`, `paused`, `temp_banned`.

Callbacks into Frappe (`yourwhatsapp.api`): `update_qr`, `inbound_message`, `message_status`, `account_event`. They go through a persistent SQLite outbox with retry and backoff, so a Frappe outage does not lose messages; permanently failed rows move to `outbox_dead`.

## Behaviour worth knowing

- Only 1:1 chats (`@c.us` / `@lid`) are relayed. Status updates, groups, broadcast lists and channels are dropped.
- Attachments are base64 in the request body (limit 25 MB); the relay never fetches URLs.
- Sessions are restored from `relay.db` on boot, so a restart does not need re-scanning.
- Never run two relays against the same data dir or the same linked number: WhatsApp replaces the stream and the two fight.
- `relay.db` holds the device keys. Anyone with a copy can act as the linked number: treat it and its backups like credentials, and never expose the relay port to the internet.
- Logs are JSON on stdout with `session_id` on per-account lines. Message bodies, media, QR codes and keys are never logged.
