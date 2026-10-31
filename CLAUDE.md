# yourwhatsapp

Self-hosted WhatsApp sending/receiving layer for a cannabis-industry CRM. Official Meta WhatsApp Business API is off the table (drugs/marijuana is a restricted category under Meta's policy), so this uses an unofficial client (`whatsapp-web.js`, puppeteer-based) behind a self-hosted Node relay instead. Document this as an internal ops tool, never as Meta-approved or "ban-proof" — see Anti-ban section below.

## Architecture

```
Frappe CRM (installed, unmodified) ──calls──> crm.api.whatsapp.*
                                                     │
                              WhatsApp Message / WhatsApp Account /
                              WhatsApp Settings / WhatsApp Templates
                                    (this app: yourwhatsapp)
                                                     │
                                    yourwhatsapp/relay_client.py
                                                     │  HTTP + x-api-key
                                                     ▼
                          Node relay (yourwhatsapp/yourwhatsapp/relay/)
                              one whatsapp-web.js Client per WhatsApp Account
                                                     │
                                                     ▼
                                          real WhatsApp (unofficial)
```

**Relay engine migration (decided 2026-10-31):** the Node/whatsapp-web.js relay is being replaced by a Go relay on **whatsmeow** (direct multi-device websocket, no headless browser), keeping the same HTTP contract with Frappe. Design: `docs/superpowers/specs/2026-10-31-whatsmeow-relay-design.md`; tracking: `ROADMAP.md` Phase 1.6 §0. Until cutover, the browser-specific gotchas below (Chromium, `ready` event, `_serialized`, user-agent) still apply to the running Node relay; after cutover they're obsolete and should be removed.

Key insight that shapes the whole design: **Frappe CRM already has a full WhatsApp integration seam** (`crm/api/whatsapp.py`, `WhatsAppBox.vue`, etc.) that works purely off doctype *names* (`WhatsApp Message`, `WhatsApp Account`, `WhatsApp Settings`, `WhatsApp Templates`) — it doesn't care which app supplies them. This app supplies doctypes with the same names/shape as the well-known `frappe_whatsapp` (Meta Cloud API) app, so CRM's frontend and hooks work unmodified, but the controller sends through the relay instead of Meta.

## Running locally

Site: `whatsapp.localhost`. Apps installed: `frappe`, `erpnext`, `crm`, `yourwhatsapp`. (`frappe_whatsapp`, `telephony`, `whatsapp_automation_pro` exist as source on this bench but are intentionally **not** installed — reference only.)

### Go relay (whatsmeow) — target engine

```bash
export PATH=$HOME/.local/go/bin:$PATH
cd apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay
make build && make test     # CGO_ENABLED=0 -> bin/relay
RELAY_API_KEY=<key> FRAPPE_BASE_URL=http://127.0.0.1:8001 RELAY_PORT=8800 \
  RELAY_DATA_DIR=$PWD/data bin/relay
```

At cutover the bench `Procfile` `relay:` line (a bench-local file, not in git; deliberately not changed yet) becomes:

```
relay: apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay/bin/relay
```

with `<bench>/.env` providing `RELAY_API_KEY`, `FRAPPE_BASE_URL=http://127.0.0.1:8001`, `RELAY_PORT=8800` and `RELAY_DATA_DIR=<abs path>/relay/data`. Env vars and routes: `yourwhatsapp/relay/README.md`. Production (Docker): `DEPLOYMENT.md`.

### Node relay (until cutover)

```bash
cd apps/yourwhatsapp/yourwhatsapp/relay
npm install
RELAY_API_KEY=<key> FRAPPE_BASE_URL=http://127.0.0.1:8001 RELAY_PORT=8800 \
  CHROMIUM_PATH=/usr/bin/chromium-headless-shell \
  node server.js
```

On this bench the relay env vars live in `<bench>/.env`, which honcho loads for the `relay` Procfile entry — `bench start` brings the relay up with everything else (currently the Node relay).

`WhatsApp Settings` needs: `relay_base_url` (e.g. `http://localhost:8800`) and `relay_api_key` (same value as `RELAY_API_KEY` above). `relay_reachable_frappe_url` is **not read by any code** — the relay uses its own `FRAPPE_BASE_URL` env var. Phase 1.6 either repurposes or removes that field.

Pairing: create a `WhatsApp Account`, then either click **Connect / Generate QR** on its form, or `POST /session/<session_id>/start` on the relay directly. Scan the QR shown on the account's `qr_code` field from a phone (WhatsApp → Linked Devices).

## Go relay gotchas

- **Go is at `$HOME/.local/go/bin`** and is not on `PATH` — `export PATH=$HOME/.local/go/bin:$PATH` first.
- **whatsmeow is pinned** in `go.mod` (`v0.0.0-20261030195514-56e5ad0581ce`). It has no tagged releases and breaks often; upgrades are deliberate, tested against real numbers.
- **One SQLite file, `relay.db`, in `RELAY_DATA_DIR`, holds every linked number's device keys** plus the callback outbox. Back it up (see `DEPLOYMENT.md`); losing it means relinking every number. Treat it as a credential.
- **Never run two relays against the same data dir (or the same linked number)** — WhatsApp answers with `StreamReplaced` and the two fight. Stop the old one first.
- **Send is asynchronous:** `POST /message/send` returns 202 `queued`; the real result arrives as a `message_status` callback.
- Docker files (`relay/Dockerfile`, `relay/compose.whatsapp.yaml`) were written without Docker on the dev host and are untested until the first real deploy.

## Hard-won gotchas — Node relay (until cutover; obsolete afterwards, then delete)

- **`.localhost` domains don't resolve from Node's DNS resolver** on this host, even though curl/browsers handle them fine. Any URL Node needs to fetch (Frappe callback base, or an outgoing attachment URL built via `frappe.utils.get_url()`) must use `http://127.0.0.1:<port>` instead — see `relay_reachable_frappe_url` in `WhatsApp Settings` and `RELAY_PORT`/`FRAPPE_BASE_URL` env vars.
- **`PORT` env var collides** with something already set in some shells — the relay's port var is named `RELAY_PORT`, not `PORT`.
- **ARM64 hosts**: Puppeteer's bundled Chromium is x86_64-only and won't run under Rosetta-less ARM containers. Set `CHROMIUM_PATH` to a native ARM Chromium (e.g. `apt install chromium-headless-shell`, then `/usr/bin/chromium-headless-shell`).
- **Frappe's `File` doctype only writes the physical file on insert**, never on update — setting `.content` on an existing `File` doc and calling `.save()` silently leaves the old bytes on disk. To "update" a file in place (e.g. rotating QR codes), delete the old `File` doc first, then reinsert with the same filename so it doesn't get auto-suffixed.
- **`whatsapp-web.js`'s `ready` event is unreliable** — it can hang indefinitely even once the account is genuinely connected. `session_manager.js` falls back to polling `client.getState()`, but only calls `attachEventListeners()` once `window.WWebJS` (the injected bridge) is actually present on the page — WhatsApp Web reloads the page right after a device links, tearing the bridge down temporarily, and calling into it too early destroys wwebjs's own in-flight injection and can corrupt an otherwise-working session.
- **Killing the relay Node process does not kill its Chromium children** — always `pkill`/kill the `chromium-headless-shell` processes under `relay/.sessions/session-<id>/` too, or you'll get orphaned browsers holding the profile lock and blocking reconnection.
- **Repeated hard kills of a connected session can get the device unlinked by WhatsApp itself** (abrupt-disconnect abuse protection) — don't casually SIGKILL a live, working session; restart the relay gracefully when possible.
- **whatsapp-web.js's default user-agent is hardcoded and outdated** (`Chrome/101.0.4951.67`, identical across every deployment of the library worldwide) — a known fingerprintable pattern. See anti-ban notes.
- **`whatsapp-web.js` 1.34.7's `message.id._serialized` is `undefined`** as of WhatsApp's July 2026 Web update — WhatsApp renamed the internal getter to `id.$1`, and the installed library version hasn't caught up. This silently broke `downloadMedia()` (opaque `r: r` error), outgoing `message_id` tracking, and ack status updates. `session_manager.js` backfills `_serialized` from `$1` via `patchSerializedId()`/`serializedId()` wherever an id object enters our code. Remove once upstream ships a fix ([wwebjs/whatsapp-web.js#201833](https://github.com/wwebjs/whatsapp-web.js/issues/201833)).
- **Never hand the relay a URL to fetch for outgoing attachments.** User-uploaded files default to private in Frappe (`/private/files/...`), which an anonymous fetch from the relay's Node process can't read — and even public files hit the `.localhost` DNS gotcha above. `relay_client.py` reads the file directly via `File.get_content()` and sends it base64-encoded instead; the relay builds a `MessageMedia` object from bytes, no fetch involved.
- **Express's default JSON body limit (100kb) silently rejects real media** once base64-encoded (~33% size inflation). `server.js` sets it to 25mb.
- **Frappe `File` records default to `is_private=0`** — anything written without setting it explicitly is served to anyone with the URL, no login. Inbound customer media and QR images both shipped public until the 2026-09-18 fix (`v1_0.make_whatsapp_files_private` migrates old rows). A live QR is a credential: whoever scans it links their own device, so QR files are private and deleted once the account connects.
- **WhatsApp emits far more than 1:1 chat messages** — contacts' status updates arrive as `status@broadcast`, plus groups (`@g.us`), broadcast lists and channels (`@newsletter`). None map to a single phone number, so CRM can't link them to a Lead/Deal; `session_manager.js` filters inbound messages *and* acks down to `@c.us`/`@lid`. Before that filter, 296 of 533 stored inbound messages were noise.
- **WhatsApp acks every outgoing message on the linked number**, including ones typed on the phone that CRM never created. `api.message_status` treats an unknown `message_id` as a no-op — raising there filled the Error Log (60 of 78 entries).
- **Desk terminal note:** stopping `bench start` with Ctrl+Z (instead of Ctrl+C) leaves the whole stack running and holding ports 8001/9001/8800 — the next `bench start` dies with `EADDRINUSE :9001`. Look for a honcho process in `T` state.

## Anti-ban

No self-hosted/unofficial client setup is "ban-proof" — this is risk *reduction*, not immunity, and must never be marketed otherwise (compliance requirement from the original spec). Full task list, in detail, lives in `ROADMAP.md` (Phase 3) — don't duplicate it here, that file is the source of truth.

## Phase status

See `ROADMAP.md` for the full phase-by-phase breakdown with objectives and detailed task lists. Short version: Phase 1 (foundation) and Phase 1.5 (hardening — media send/receive, private files, several real relay/wwebjs bugs) are done and verified against real WhatsApp numbers. Phase 1.6 (production readiness: Docker deploy, UI-only relay setup, security/reliability blockers) is in progress and blocks go-live; Phase 2 (trigger-based auto-messaging) comes after.
