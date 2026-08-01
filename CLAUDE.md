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

Key insight that shapes the whole design: **Frappe CRM already has a full WhatsApp integration seam** (`crm/api/whatsapp.py`, `WhatsAppBox.vue`, etc.) that works purely off doctype *names* (`WhatsApp Message`, `WhatsApp Account`, `WhatsApp Settings`, `WhatsApp Templates`) — it doesn't care which app supplies them. This app supplies doctypes with the same names/shape as the well-known `frappe_whatsapp` (Meta Cloud API) app, so CRM's frontend and hooks work unmodified, but the controller sends through the relay instead of Meta.

## Running locally

Site: `whatsapp.localhost`. Apps installed: `frappe`, `erpnext`, `draw`, `flowlane`, `crm`, `yourwhatsapp`. (`frappe_whatsapp`, `telephony`, `whatsapp_automation_pro` exist as source on this bench but are intentionally **not** installed — reference only.)

```bash
cd apps/yourwhatsapp/yourwhatsapp/relay
npm install
RELAY_API_KEY=<key> FRAPPE_BASE_URL=http://127.0.0.1:8001 RELAY_PORT=8800 \
  CHROMIUM_PATH=/usr/bin/chromium-headless-shell \
  node server.js
```

`WhatsApp Settings` needs: `relay_base_url` (e.g. `http://localhost:8800`), `relay_api_key` (same value as `RELAY_API_KEY` above), and `relay_reachable_frappe_url` (e.g. `http://127.0.0.1:8001` — see gotcha below).

Pairing: create a `WhatsApp Account`, then either click **Connect / Generate QR** on its form, or `POST /session/<session_id>/start` on the relay directly. Scan the QR shown on the account's `qr_code` field from a phone (WhatsApp → Linked Devices).

## Hard-won gotchas (read before touching the relay)

- **`.localhost` domains don't resolve from Node's DNS resolver** on this host, even though curl/browsers handle them fine. Any URL Node needs to fetch (Frappe callback base, or an outgoing attachment URL built via `frappe.utils.get_url()`) must use `http://127.0.0.1:<port>` instead — see `relay_reachable_frappe_url` in `WhatsApp Settings` and `RELAY_PORT`/`FRAPPE_BASE_URL` env vars.
- **`PORT` env var collides** with something already set in some shells — the relay's port var is named `RELAY_PORT`, not `PORT`.
- **ARM64 hosts**: Puppeteer's bundled Chromium is x86_64-only and won't run under Rosetta-less ARM containers. Set `CHROMIUM_PATH` to a native ARM Chromium (e.g. `apt install chromium-headless-shell`, then `/usr/bin/chromium-headless-shell`).
- **Frappe's `File` doctype only writes the physical file on insert**, never on update — setting `.content` on an existing `File` doc and calling `.save()` silently leaves the old bytes on disk. To "update" a file in place (e.g. rotating QR codes), delete the old `File` doc first, then reinsert with the same filename so it doesn't get auto-suffixed.
- **`whatsapp-web.js`'s `ready` event is unreliable** — it can hang indefinitely even once the account is genuinely connected. `session_manager.js` falls back to polling `client.getState()`, but only calls `attachEventListeners()` once `window.WWebJS` (the injected bridge) is actually present on the page — WhatsApp Web reloads the page right after a device links, tearing the bridge down temporarily, and calling into it too early destroys wwebjs's own in-flight injection and can corrupt an otherwise-working session.
- **Killing the relay Node process does not kill its Chromium children** — always `pkill`/kill the `chromium-headless-shell` processes under `relay/.sessions/session-<id>/` too, or you'll get orphaned browsers holding the profile lock and blocking reconnection.
- **Repeated hard kills of a connected session can get the device unlinked by WhatsApp itself** (abrupt-disconnect abuse protection) — don't casually SIGKILL a live, working session; restart the relay gracefully when possible.
- **whatsapp-web.js's default user-agent is hardcoded and outdated** (`Chrome/101.0.4951.67`, identical across every deployment of the library worldwide) — a known fingerprintable pattern. See anti-ban notes.

## Anti-ban (production readiness — not yet fully implemented, tracked as pending work)

No self-hosted/unofficial client setup is "ban-proof" — this is risk *reduction*, not immunity, and must never be marketed otherwise (compliance requirement from the original spec). Open items:

- Fix the hardcoded default UA (shared across every whatsapp-web.js install, outdated).
- IP diversity per number — all accounts currently share one relay host/IP, a known farm-detection signal.
- Reduce headless/puppeteer fingerprint distinctiveness.
- Number provisioning: real SIM-registered numbers, gradual volume warm-up per new number.
- Enforce per-number daily send caps (`daily_limit` field exists, not yet a hard stop) + randomized inter-send delay.
- Avoid identical bulk-blast content across numbers/contacts once trigger-based messaging (phase 2) exists.
- Health scoring + auto-pause *before* a number gets banned, not after (spec's "Sender Health and Protection Controls" — still unbuilt).
- Cooldown/auto-recovery timer for accounts stuck in `RateLimited` status.

## Known bugs in progress

- Outbound media (image/video/document) send — fixed: was using `frappe.utils.get_url()` which returns a `.localhost` URL Node can't resolve; now uses `relay_reachable_frappe_url` from `WhatsApp Settings`.
- Inbound media — fixed: `session_manager.js`'s message handler now calls `msg.downloadMedia()` and `api.py`'s `inbound_message` saves it as a `File` attached to the new `WhatsApp Message`. Needs relay restart + live retest to confirm.

## Phase status

Phase 1 (foundation — accounts, routing/failover, relay, CRM integration, send/receive) is built and verified against real WhatsApp numbers. Phase 2 (trigger-based auto-messaging, health/protection controls, ops dashboard, audit trail) is not yet started — trigger-based auto-messaging was chosen as the next priority, but anti-ban and media bugs took precedence once real-number testing surfaced them.
