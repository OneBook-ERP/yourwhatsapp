# yourwhatsapp relay

Self-hosted WhatsApp sending/receiving layer, backed by `whatsapp-web.js`. Runs as a standalone Node process alongside the Frappe bench — not part of gunicorn/workers. One `whatsapp-web.js` client (and one Chromium) per paired number.

## Setup

```bash
cd apps/yourwhatsapp/yourwhatsapp/relay
npm install
```

## Environment variables

- `RELAY_API_KEY` — shared secret, must match `WhatsApp Settings.relay_api_key` on the Frappe site. Required; the relay exits without it.
- `FRAPPE_BASE_URL` — base URL the relay calls back on (e.g. `http://127.0.0.1:8001`). Must be a URL Node can resolve — `.localhost` names don't resolve from Node on some hosts, use `127.0.0.1:<port>`.
- `RELAY_PORT` — relay HTTP port (default `8800`). Named `RELAY_PORT`, not `PORT`, which collides with other tooling.
- `CHROMIUM_PATH` — path to a system Chromium binary. Puppeteer's bundled Chromium is x86_64-only; on ARM hosts (e.g. Apple Silicon under Docker) it won't run — point this at a native ARM Chromium instead (e.g. `/usr/bin/chromium-headless-shell` on Debian/Ubuntu with `chromium-headless-shell` installed).

On a dev bench these are read from `<bench>/.env`, which honcho loads for the `relay` Procfile entry, so `bench start` brings the relay up with the rest of the bench.

## Run

```bash
RELAY_API_KEY=<key> FRAPPE_BASE_URL=http://127.0.0.1:8001 npm start
```

## Endpoints

All require the `x-api-key` header.

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/session/:id/start` | Initialize a client, trigger QR generation |
| GET | `/session/:id/status` | `not_started` / `initializing` / `qr_pending` / `connected` / `disconnected` |
| POST | `/session/:id/pause` | Destroy the client, keep the stored session |
| POST | `/session/:id/resume` | Start it again |
| POST | `/message/send` | Send text or media (media as base64 in the body) |

Callbacks into Frappe (`api.py`): `update_qr`, `inbound_message`, `message_status`.

## Behaviour worth knowing

- **Only 1:1 chats are relayed.** Status updates (`status@broadcast`), groups (`@g.us`), broadcast lists and channels are ignored in both directions — they have no single counterpart number for CRM to link to a Lead or Deal.
- **Delivery/read receipts for messages CRM didn't send** (typed on the phone, for example) are ignored by Frappe as a no-op rather than treated as an error.
- **Attachments are never fetched by the relay.** Frappe reads the file and sends the bytes base64-encoded, so private files and unresolvable URLs are not a problem. The JSON body limit is 25mb for this reason.
- **Sessions are not restored when the relay restarts** — each account must currently be reconnected from its form. Fixing this is a Phase 1.6 blocker.
- **Killing the relay does not kill its Chromium children.** Terminate the `chromium-headless-shell` processes under `.sessions/session-<id>/` too, or the leftover browsers hold the profile lock. Avoid hard-killing a live session: WhatsApp can unlink a device that keeps disconnecting abruptly.

## Pairing a number

1. Create a `WhatsApp Account` doc in Frappe (generates a `session_id`).
2. `POST /session/<session_id>/start` (with `x-api-key` header) to initialize the client and trigger QR generation, or click **Connect / Generate QR** on the account form.
3. The QR is pushed back to the `WhatsApp Account.qr_code` field as a **private** file — scan it from the WhatsApp mobile app (Linked Devices). A live QR is a credential: anyone who scans it links their own device to the number.
4. On successful pairing, `WhatsApp Account.status` flips to `Active` and the QR file is deleted.

## Security

- Never expose the relay port to the internet. Anyone who can reach it with the key can send messages as your numbers.
- `.sessions/` holds logged-in WhatsApp sessions. Copying that directory is enough to use the numbers without scanning anything — restrict access and treat backups of it like credentials.
