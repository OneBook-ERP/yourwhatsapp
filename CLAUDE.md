# yourwhatsapp

Self-hosted WhatsApp sending/receiving layer for a cannabis-industry CRM. Official Meta WhatsApp Business API is off the table (drugs/marijuana is a restricted category under Meta's policy), so this uses an unofficial client (**whatsmeow**, WhatsApp's multi-device protocol over a websocket, no browser) behind a self-hosted Go relay instead. Document this as an internal ops tool, never as Meta-approved or "ban-proof" — see Anti-ban section below.

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
                          Go relay (yourwhatsapp/yourwhatsapp/relay/)
                              one whatsmeow Client per WhatsApp Account
                                                     │
                                                     ▼
                                          real WhatsApp (unofficial)
```

**Relay engine:** the original Node/whatsapp-web.js relay was replaced by the Go/whatsmeow relay (decided and cut over 2026-10-31; Node files deleted 2026-11-02). Designs: `docs/superpowers/specs/2026-10-31-whatsmeow-relay-design.md` (relay) and `docs/superpowers/specs/2026-10-31-phase-1-6-blockers-design.md` (claim flow, template parameters, default number).

**Client requirement — context belongs to the agent, not the number:** the agent in the CRM must always see the full conversation with a customer, even when the WhatsApp number in use changes (rotation, failover, a number banned or swapped). The customer may see messages arrive from different numbers — accepted. So: **no sticky sender** (every outgoing message, replies included, rotates via `routing.pick_account`); threads are keyed by the customer (CRM Lead/Deal + normalized digits-only phone number), never by `whatsapp_account`. **Exception: reactions.** WhatsApp only applies a reaction sent from the number whose chat holds the target message, so a reaction is pinned to the target message's `whatsapp_account` (`WhatsApp Message._pin_reaction_account`). If that account isn't Active it fails at once ("Reaction needs <account> to be connected") and never fails over. Quoted replies still rotate (the quote carries its own text).

Key insight that shapes the whole design: **Frappe CRM already has a full WhatsApp integration seam** (`crm/api/whatsapp.py`, `WhatsAppBox.vue`, etc.) that works purely off doctype *names* (`WhatsApp Message`, `WhatsApp Account`, `WhatsApp Settings`, `WhatsApp Templates`) — it doesn't care which app supplies them. This app supplies doctypes with the same names/shape as the well-known `frappe_whatsapp` (Meta Cloud API) app, so CRM's frontend and hooks work unmodified, but the controller sends through the relay instead of Meta.

## Running locally

Site: `whatsapp.localhost`. Apps installed: `frappe`, `erpnext`, `crm`, `yourwhatsapp`. (`frappe_whatsapp`, `telephony`, `whatsapp_automation_pro` exist as source on this bench but are intentionally **not** installed — reference only.)

### Relay (Go / whatsmeow)

```bash
export PATH=$HOME/.local/go/bin:$PATH
cd apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay
make build && make test     # CGO_ENABLED=0 -> bin/relay
RELAY_API_KEY=<key> FRAPPE_BASE_URL=http://127.0.0.1:8001 RELAY_PORT=8800 \
  RELAY_DATA_DIR=$PWD/data bin/relay
```

The bench `Procfile` (bench-local, not in git) already runs the Go relay since the 2026-10-31 cutover:

```
relay: apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay/bin/relay
```

`<bench>/.env` provides `RELAY_API_KEY`, `FRAPPE_BASE_URL=http://127.0.0.1:8001`, `RELAY_PORT=8800` and `RELAY_DATA_DIR=<abs path>/relay/data`. Because `RELAY_API_KEY` is set, the dev relay runs in **env mode** ("pre-claimed"): `/claim` answers 409, and `/claim/rotate` and `/claim/callback` answer 409 `env_mode`. Production relays normally start **unclaimed** and are claimed from Settings (see `DEPLOYMENT.md`). Modes, routes and env vars: `yourwhatsapp/relay/README.md`.

On this bench the relay env vars live in `<bench>/.env`, which honcho loads for the `relay` Procfile entry — `bench start` brings the relay up with everything else. Restarting bench (Ctrl+C, `bench start`) restarts the relay gracefully; linked numbers come back without a rescan.

`WhatsApp Settings` needs `relay_base_url` (e.g. `http://localhost:8800`) and `relay_api_key`. For the env-mode dev relay, type the `RELAY_API_KEY` value into `relay_api_key` by hand (the panel says so). For a claimed relay, **Connect Relay** generates and stores the key. `relay_reachable_frappe_url` is the **Callback URL** sent to the relay on claim (and by **Update Callback URL**); empty means `frappe.utils.get_url()`. On this host use `http://127.0.0.1:<port>`, since `.localhost` names don't resolve from the relay process. In env mode the relay ignores it and uses its own `FRAPPE_BASE_URL` / `FRAPPE_SITE_NAME`. The claim UI needs MariaDB (`GET_LOCK`). Template `{{n}}` placeholders are mapped to reference-document fields on the template's Parameters table; sending rotates across Active numbers, and the Settings default accounts are kept pointing at an Active number: CRM shows WhatsApp only while `default_outgoing_account` is Active, so they are refilled whenever they are empty, dangling or point at a number that left Active (disconnected, rate-limited, paused, logged out, deleted). With no Active number left they stay as they are.

Pairing: create a `WhatsApp Account`, then either click **Connect / Generate QR** on its form, or `POST /session/<session_id>/start` on the relay directly. Scan the QR shown on the account's `qr_code` field from a phone (WhatsApp → Linked Devices).

## Go relay gotchas

- **Go is at `$HOME/.local/go/bin`** and is not on `PATH` — `export PATH=$HOME/.local/go/bin:$PATH` first.
- **whatsmeow is pinned** in `go.mod` (`v0.0.0-20261030195514-56e5ad0581ce`). It has no tagged releases and breaks often; upgrades are deliberate, tested against real numbers.
- **One SQLite file, `relay.db`, in `RELAY_DATA_DIR`, holds every linked number's device keys** plus the callback outbox. Back it up (see `DEPLOYMENT.md`); losing it means relinking every number. Treat it as a credential.
- **Never run two relays against the same data dir (or the same linked number)** — WhatsApp answers with `StreamReplaced` and the two fight. Stop the old one first.
- **Send is asynchronous:** `POST /message/send` returns 202 `queued`; the real result arrives as a `message_status` callback.
- **whatsmeow only auto-reconnects a connection that once existed.** Incident 2026-11-02: after a bench restart `Connect()` failed for both restored numbers, they sat `disconnected` for good, and Frappe still showed them Active. The relay now retries a paired session itself (`internal/session/reconnect.go`: 5, 10, 20, 40, 80, 160 s, then every 300 s, ±20 %) until it connects, is paused/logged out/retired or the relay stops; `StreamReplaced` never retries; `ClientOutdated` stops at once, pushes Disconnected and an `account_event` `client_outdated` (Frappe logs "WhatsApp client outdated": upgrade whatsmeow); a temp ban keeps its own timer. Start/Resume on a disconnected paired session re-dials the same client unless the proxy changed. Frappe hears `Disconnected` only once a session has been down 60 s straight, and `tasks.reconcile_account_status` (every 5 min) re-syncs Active/Disconnected (Disconnected only after the relay has reported it down for a full interval) and `last_health_check` from `GET /session/<id>/status` (`relay_client.session_status`) in case a callback was lost — it never overrides Paused or a held ban, and stops at the first connection error.
- **`/claim` and `PUT /claim/callback` call Frappe back first** (`api.ping` at the candidate callback URL, with the candidate key and `Host: <site_name>`); an unreachable target is refused with 400 `callback_unreachable`. The Frappe web server must be able to serve that ping while the claim request is still open (fine with threaded `bench serve` or more than one gunicorn worker).
- **A 401 on send never fails a message:** `relay_client` adopts a pending key the relay accepts, else parks the message for the relay-unavailable retry.
- **Relay connection fields are System Manager only, enforced in `WhatsApp Settings.validate`**; a typed `relay_api_key` saves only while the relay is in env mode. Tests set keys with `write_password`, never via `settings.save()`.
- **Health alerts (`yourwhatsapp/alerts.py`):** `alerts.account_status_changed(account, old, new, reason)` is called from the routing helpers and the `client_outdated` handler. It fires only on a real transition where one side is Active: a bell (Notification Log) plus email to enabled System Manager / WhatsApp Admin users. Down alerts are throttled to one per account per 15 min (Redis key); a recovery ("back online") is sent only if a down alert was sent and is never throttled. Manual Pause/Logout pass `reason="manual"`, are silent, and set a 5-minute flag so the relay's following Disconnected doesn't alert either. No outgoing email account: bell only, one Error Log note per day.
- **Tests run on the live site (`whatsapp.localhost`) and must never commit.** Every test class subclasses `tests.utils.LiveSafeTestCase` (patches `frappe.db.commit`, mocks `frappe.sendmail`, `frappe.log_error` and `frappe.publish_realtime`, isolates alert Redis keys, rolls back even when setUp fails); `test_live_safety.py` fails if a module doesn't. Snapshot the live data before and diff after a run. `tabError Log` is MyISAM (Frappe's own choice), so a real `frappe.log_error` row is written at once and no rollback removes it — that is why the base mocks it; assert on `self.log_error_mock`. Frappe's test runner disables the scheduler for the run and re-enables it at the end; if a run is killed, run `bench --site whatsapp.localhost enable-scheduler`.
- **Docs:** RBAC audit in `docs/rbac-audit.md` (audit only, no permission changes made), warm-up policy in `docs/warm-up.md`, media size limit chain in `DEPLOYMENT.md`.
- Docker files (`relay/Dockerfile`, `relay/compose.whatsapp.yaml`) were written without Docker on the dev host and are untested until the first real deploy.

## Hard-won gotchas

- **`.localhost` domains don't resolve from the relay process** on this host, even though curl/browsers handle them fine. Any URL the relay calls (the Frappe callback base) must use `http://127.0.0.1:<port>` — see the Callback URL (`relay_reachable_frappe_url`) in `WhatsApp Settings` and the `FRAPPE_BASE_URL` env var.
- **`PORT` env var collides** with something already set in some shells — the relay's port var is named `RELAY_PORT`, not `PORT`.
- **Frappe's `File` doctype only writes the physical file on insert**, never on update — setting `.content` on an existing `File` doc and calling `.save()` silently leaves the old bytes on disk. To "update" a file in place (e.g. rotating QR codes), delete the old `File` doc first, then reinsert with the same filename so it doesn't get auto-suffixed.
- **Repeated hard kills of a connected session can get the device unlinked by WhatsApp itself** (abrupt-disconnect abuse protection) — never SIGKILL the relay; stop it with SIGTERM/Ctrl+C so it disconnects every client cleanly.
- **Never hand the relay a URL to fetch for outgoing attachments.** User-uploaded files default to private in Frappe (`/private/files/...`), which an anonymous fetch from the relay can't read — and even public files hit the `.localhost` DNS gotcha above. `relay_client.py` reads the file directly via `File.get_content()` and sends it base64-encoded instead; the relay uploads the bytes to WhatsApp directly, no fetch involved.
- **Frappe `File` records default to `is_private=0`** — anything written without setting it explicitly is served to anyone with the URL, no login. Inbound customer media and QR images both shipped public until the 2026-09-18 fix (`v1_0.make_whatsapp_files_private` migrates old rows). A live QR is a credential: whoever scans it links their own device, so QR files are private and deleted once the account connects.
- **WhatsApp emits far more than 1:1 chat messages** — contacts' status updates arrive as `status@broadcast`, plus groups (`@g.us`), broadcast lists and channels (`@newsletter`). None map to a single phone number, so CRM can't link them to a Lead/Deal; the relay (`internal/convert`) filters inbound messages *and* receipts down to 1:1 chats (`s.whatsapp.net`/`lid`). Before that filter, 296 of 533 stored inbound messages were noise.
- **WhatsApp acks every outgoing message on the linked number**, including ones typed on the phone that CRM never created. `api.message_status` treats an unknown `message_id` as a no-op — raising there filled the Error Log (60 of 78 entries).
- **Desk terminal note:** stopping `bench start` with Ctrl+Z (instead of Ctrl+C) leaves the whole stack running and holding ports 8001/9001/8800 — the next `bench start` dies with `EADDRINUSE :9001`. Look for a honcho process in `T` state.

## Anti-ban

No self-hosted/unofficial client setup is "ban-proof" — this is risk *reduction*, not immunity, and must never be marketed otherwise (compliance requirement from the original spec). Full task list, in detail, lives in `ROADMAP.md` (Phase 3) — don't duplicate it here, that file is the source of truth.

## Phase status

See `ROADMAP.md` for the full phase-by-phase breakdown with objectives and detailed task lists. Short version: Phase 1 (foundation) and Phase 1.5 (hardening) are done. Phase 1.6 (production readiness): the Go relay, claim-based UI setup, template parameters and default-number cleanup are done and verified live on the dev bench; the remaining items (status reconciliation, replies/reactions, RBAC audit, health alerts, warm-up doc) are plan 2 and still block go-live; Phase 2 (trigger-based auto-messaging) comes after.
