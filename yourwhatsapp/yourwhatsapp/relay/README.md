# yourwhatsapp relay

Self-hosted WhatsApp sending/receiving layer, backed by `whatsapp-web.js`. Runs as a standalone Node process alongside the Frappe bench — not part of gunicorn/workers.

## Setup

```bash
cd apps/yourwhatsapp/yourwhatsapp/relay
npm install
```

## Environment variables

- `RELAY_API_KEY` — shared secret, must match `WhatsApp Settings.relay_api_key` on the Frappe site.
- `FRAPPE_BASE_URL` — base URL of the site (e.g. `http://whatsapp.localhost:8000`).
- `RELAY_PORT` — relay HTTP port (default `8800`).
- `CHROMIUM_PATH` — path to a system Chromium binary. Puppeteer's bundled Chromium is x86_64-only; on ARM hosts (e.g. Apple Silicon under Docker) it won't run — point this at a native ARM Chromium instead (e.g. `/usr/bin/chromium-headless-shell` on Debian/Ubuntu with `chromium-headless-shell` installed).

## Run

```bash
RELAY_API_KEY=<key> FRAPPE_BASE_URL=http://whatsapp.localhost:8000 npm start
```

## Pairing a number

1. Create a `WhatsApp Account` doc in Frappe (generates a `session_id`).
2. `POST /session/<session_id>/start` (with `x-api-key` header) to initialize the client and trigger QR generation.
3. The QR is pushed back to the `WhatsApp Account.qr_code` field — scan it from the WhatsApp mobile app (Linked Devices).
4. On successful pairing, `WhatsApp Account.status` flips to `Active` automatically.
