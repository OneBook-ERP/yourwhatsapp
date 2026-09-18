# yourwhatsapp — Roadmap

Tracks phases against the original spec: an internal WhatsApp communication and CRM system for a cannabis-industry client, self-hosted (no official Meta Business API), with multi-number management, routing/failover, trigger-based messaging, sender health/protection controls, and full CRM integration.

Each phase lists an objective, detailed tasks, and status. Update statuses as work lands — this file is the source of truth for "what's next," not a snapshot to archive.

---

## Phase 1 — Foundation ✅ Done

**Objective:** Prove the core pipe end-to-end — a self-hosted relay that can pair a number, send, and receive, integrated into CRM without CRM-side changes.

- [x] `WhatsApp Account` / `WhatsApp Settings` / `WhatsApp Message` / `WhatsApp Templates` doctypes, shaped to match Frappe CRM's existing `crm.api.whatsapp` integration seam so CRM's frontend works unmodified.
- [x] Node relay (`relay/`) on `whatsapp-web.js`, one `Client` per account, `LocalAuth` session persistence.
- [x] QR pairing flow, `Connect / Generate QR` button on the account form with auto-polling.
- [x] Round-robin / priority routing across multiple accounts (`routing.py`).
- [x] Single-hop failover to the next eligible account on send failure.
- [x] Inbound/outbound webhook bridge (`api.py`: `inbound_message`, `message_status`, `update_qr`).
- [x] Verified against real WhatsApp numbers: pairing, text send, text receive, `@lid` → phone number resolution, round-robin alternation, failover.

## Phase 1.5 — Hardening ✅ Done

**Objective:** Fix the real bugs that only surfaced once real numbers were connected under real conditions — this phase wasn't planned, it was forced by testing.

- [x] `WhatsApp Message.status` field default fighting the controller's own status-setting logic.
- [x] `relay_client.send_message` crashing silently instead of marking a message `Failed` when the relay isn't configured.
- [x] `sendMessage()` return-value handling (wwebjs can resolve without a `Message` object on a successful send).
- [x] QR image churn: Frappe's `File` doctype only writes the physical file on insert, never on update — rotating QR codes was piling up hundreds of `File` records and colliding on concurrent writes. Fixed via delete-then-reinsert with cache-busted URLs.
- [x] `attachEventListeners()` race: calling it before `window.WWebJS` (wwebjs's injected bridge) is actually present destroys wwebjs's own in-flight page injection and can corrupt a working session. Fixed with a readiness gate + retry-until-success instead of a tight retry loop.
- [x] Outbound media send: was building a fetchable URL for the relay (broke on private Frappe files with no relay-side auth, and on `.localhost` URLs Node can't resolve). Fixed by reading the file directly in Python and sending it base64-encoded.
- [x] Express's default 100kb JSON body limit rejecting any real image/video/PDF once base64-encoded. Raised to 25mb.
- [x] Inbound media never downloaded at all — `session_manager.js` set `content_type` correctly but never called `msg.downloadMedia()`. Fixed, media now saved as a `File` on the incoming `WhatsApp Message`.
- [x] Worked around an open upstream `whatsapp-web.js` 1.34.7 bug: WhatsApp's July 2026 Web update renamed the internal serialized-id getter (`_serialized` → `$1`), silently breaking `downloadMedia()`, outgoing `message_id` tracking, and ack status updates. Backfilled in our own code (tracked upstream: [wwebjs/whatsapp-web.js#201833](https://github.com/wwebjs/whatsapp-web.js/issues/201833)).

## Phase 1.6 — Production Readiness 🔄 In progress (blocks go-live, ahead of Phase 2)

**Objective:** Make the existing send/receive system safe and operable in production. Production runs in Docker with no shell access for us (no `bench`/`docker exec`), so everything after the initial compose deploy must be configurable from the Desk UI by a System Manager. 🚫 = must fix before go-live; the rest should land before real volume.

### A. Docker deployment + UI-only setup
- [ ] 🚫 Relay Docker image (`relay/Dockerfile`): Node + system Chromium baked in (no `CHROMIUM_PATH` to configure), `tini`/`init: true` so Chromium children get reaped, `/app/.sessions` as a volume.
- [ ] 🚫 Example compose service (`whatsapp-relay`): no published port (internal docker network only), sessions volume, `stop_grace_period: 30s`, healthcheck.
- [ ] 🚫 **Relay claim flow — no terminal needed for the key.** Relay boots unclaimed (no `RELAY_API_KEY` env required). System Manager enters relay URL in `WhatsApp Settings` → **Connect Relay** → Frappe generates a random key, POSTs it to relay `/claim` along with its callback URL + site name; relay persists it in its volume and rejects any further claims. Key stored in `relay_api_key` (Password). Optional `RELAY_BOOTSTRAP_TOKEN` env to require a token for claiming. Reset = wipe relay volume.
- [ ] 🚫 Relay sends callbacks with `Host: <site name>` so multi-site benches route correctly; callback URL comes from the claim, replacing the `FRAPPE_BASE_URL` env var. Repurpose (or remove) the currently-unused `relay_reachable_frappe_url` field.
- [ ] Settings page relay panel: connection status, relay version, session count, **Test Connection**, **Rotate Key**.
- [ ] `GET /health` on the relay (unauthenticated, no data) for the docker healthcheck.
- [ ] `RELAY_HOST` bind option (default `0.0.0.0` in Docker, `127.0.0.1` for bare-metal installs).
- [ ] Setup guide (`DEPLOYMENT.md`): compose snippet, Settings walkthrough, pairing, backup of the sessions volume.

### B. Session lifecycle (Docker restarts containers often)
- [ ] 🚫 Auto-restore every paired session on relay boot (from `.sessions/`) — today a relay restart leaves all accounts `Active` in Frappe but unable to send until manually reconnected.
- [ ] 🚫 `Connect` must work after a disconnect — `startSession` returns the stale map entry, `disconnected` never removes it.
- [ ] 🚫 Graceful SIGTERM handler: `client.destroy()` every session before exit. Repeated abrupt kills can get a device unlinked by WhatsApp (see CLAUDE.md gotchas).
- [ ] Status reconciliation: scheduler job polls relay `/session/:id/status` and syncs `WhatsApp Account.status` + `last_health_check` (field exists, unused).
- [ ] UI buttons on `WhatsApp Account`: **Pause**, **Logout / Unlink**.

### C. Security
- [ ] 🚫 Inbound media is saved as **public** files (210 customer images/docs/videos already public on dev) — save as `is_private=1`; migration patch to flip existing ones.
- [ ] 🚫 QR codes are **public** at predictable URLs (`/files/<account name>-qr.png`) — anyone guessing the URL during pairing can link their own device to the account. Make private + randomized filename, or serve via a permission-checked endpoint; delete the QR file once connected.
- [ ] Constant-time API key comparison on both sides (`hmac.compare_digest` / `crypto.timingSafeEqual`).
- [ ] RBAC audit: who can create/read `WhatsApp Message` (Sales User currently has write), who can see `WhatsApp Account`/`Settings`; `connect()` role check vs doctype perms.
- [ ] Confirm nginx `client_max_body_size` in the Frappe container allows inbound media callbacks (base64 up to ~22MB).

### D. Reliability
- [ ] 🚫 Relay→Frappe callbacks are log-and-drop (`frappe_client.js`) — an inbound message arriving while Frappe restarts during a deploy is lost forever. Persist to a small on-disk queue in the relay volume, retry with backoff.
- [ ] Failed outgoing messages: **Retry** button + automatic retry when the failure was "relay/session unavailable" (not for invalid numbers).
- [ ] Ack race: an ack can arrive before `message_id` is saved (now silently ignored) — retry briefly or have the relay return the id synchronously before ack listeners fire; make status monotonic (`Read` must not be overwritten by a late `Delivered`/`Sent`).
- [ ] Atomic `daily_message_count` increment (`UPDATE ... SET x = x + 1`) — current read-modify-write loses counts under concurrent workers.
- [ ] Dedupe inbound by `message_id`.
- [ ] Pin `whatsapp-web.js` to an exact version + `npm ci` in the image — WhatsApp Web changes already broke 1.34.7 once (July 2026); upgrades must be deliberate and tested.

### E. Functional bugs (from 2026-09-18 audit)
- [x] Status updates (`status@broadcast`), groups (`@g.us`), broadcast lists and channels no longer stored as inbound CRM messages — relay filters to 1:1 chats only. Existing rows kept.
- [x] Acks for messages CRM didn't send (typed on the phone, groups) no longer raise `DoesNotExistError` into Error Log.
- [ ] 🚫 Normalize outgoing numbers to digits only (`+91 98765 43210` → `919876543210`) and validate with `client.getNumberId()` before sending — `+`-prefixed numbers currently fail with 502.
- [ ] 🚫 `crm.api.whatsapp.send_whatsapp_template` sends no parameters → templates go out with literal `{{1}}`. Resolve params from the reference doc (or block templates with unresolved placeholders).
- [ ] Reactions (`content_type: reaction`) are sent as a plain text emoji — use `msg.react()` on the target message.
- [ ] Replies: pass `quotedMessageId` on outgoing replies; send `reply_to_message_id` on inbound quoted messages so CRM shows reply context.
- [ ] Unresolvable `@lid` senders are stored as a fake phone number (e.g. `157844527657214`) — store the raw lid in a separate field and flag it instead.
- [ ] `default_outgoing_account` (Settings) vs `is_default_outgoing` (Account) duplication; CRM's `is_whatsapp_enabled` requires the Settings one — routing ignores both. Pick one and document it; setup must set it or CRM hides WhatsApp entirely.

### F. Minimum anti-ban before real volume (subset of Phase 3)
- [ ] Per-account realistic, current user-agent instead of wwebjs's hardcoded `Chrome/101`.
- [ ] Randomized inter-send delay per account (queue per account instead of immediate dispatch).
- [ ] `RateLimited` cooldown → auto-return to `Active`.
- [ ] Warm-up policy documented for new numbers (low `daily_limit`, raise over 1–2 weeks).

### G. Ops / quality
- [ ] Tests: routing, `api.py` endpoints (auth, unknown ids, inbound media), `relay_client` failover. None exist today.
- [ ] Structured JSON logs from the relay to stdout (Docker log driver picks them up).
- [ ] Health alert (Notification / System Console) when an account goes `Disconnected`.

## Phase 2 — Trigger-Based Automated Messaging 🔲 Not started (next after Phase 1.6)

**Objective:** This is the actual point of the spec — turn the system from a manual sender into a CRM-event-driven ops tool. Everything built so far is plumbing for this.

- [ ] `WhatsApp Trigger` doctype: `document_type` (Link → DocType), `condition` (safe_eval expression, mirroring `whatsapp_automation_pro`'s reference pattern), `phone_field`, `template` (Link → WhatsApp Templates), `enabled`.
- [ ] Wire `doc_events` for the first concrete triggers: `CRM Lead.after_insert` (welcome message), `CRM Deal` stage change (status update). Confirm exact field names against the installed `crm` app's `CRM Deal`/`CRM Lead` doctypes before implementing.
- [ ] Template parameter substitution reusing the existing `{{1}}`-style convention already used by `crm.api.whatsapp.parse_template_parameters` and `WhatsApp Templates`.
- [ ] Automation log: a record of *why* and *when* each trigger fired (spec explicitly asks for this — "Automation logs showing why and when a message was triggered").
- [ ] Guard against trigger loops/duplicates (e.g. a stage-change trigger re-firing on unrelated saves).
- [ ] Route trigger-fired sends through the same routing/failover path as manual sends — no separate code path.
- [ ] Respect per-number daily caps and inter-send delay (depends on Phase 3 items #16 below — trigger-driven bulk sends are exactly the scenario that makes those matter).

## Phase 3 — Sender Health & Anti-Ban Protection 🔲 Not started

**Objective:** The spec's named "Sender Health and Protection Controls" requirement. No unofficial-client setup is ban-proof — this is risk reduction, and it's what keeps numbers alive long enough to be useful. Never document or market this phase's output as ban-proof (spec's explicit compliance requirement).

- [ ] Fix the hardcoded default `whatsapp-web.js` user-agent (`Chrome/101.0.4951.67`, identical across every deployment of the library worldwide, outdated for 2026) — set a distinct, current, realistic UA per account.
- [ ] IP diversity per number — all accounts currently share one relay host/IP, a known farm-detection signal. Evaluate per-number egress IP (residential/mobile proxy) for production scale; check current host IP class (datacenter vs residential).
- [ ] Reduce headless/puppeteer fingerprint distinctiveness — review/minimize unnecessary `--disable-*` launch args.
- [ ] Number provisioning policy: real SIM-registered numbers only (not VOIP), gradual volume warm-up over 1-2 weeks per new number. Operational policy, not just code.
- [ ] Enforce `daily_limit` as a hard stop in routing (currently only partially applied via `get_eligible_accounts`), plus randomized inter-send delay instead of fixed/instant dispatch.
- [ ] Avoid identical bulk-blast content across numbers/contacts once Phase 2's trigger-based messaging exists — enforce template variation/personalization.
- [ ] Health scoring: track disconnect/`auth_failure` frequency and error rate per account, auto-pause *before* a number gets banned, not after.
- [ ] Cooldown/auto-recovery timer for accounts stuck in `RateLimited` status — currently nothing brings them back to `Active` automatically.

## Phase 4 — Operational Controls & Dashboard 🔲 Not started

**Objective:** Make the system operable and debuggable by non-technical staff — the spec's "Operational controls" section.

- [ ] Sender account dashboard: live connection/session status, health score (from Phase 3), queue depth, daily usage vs. cap, per number.
- [ ] Failed message review with retry controls.
- [ ] Manual sender prioritization and exclusion (temporarily pull a number out of routing rotation without disconnecting it).
- [ ] Health alerts for disconnected/unstable numbers (surface Phase 3's health scoring somewhere staff will actually see it).
- [ ] Audit trail for all message and routing actions — which account handled a send, why routing picked it, any failover hops.
- [ ] Cross-account search/filter by phone number, sender account, message type, delivery state (CRM's own `get_whatsapp_messages` is scoped to one Lead/Deal at a time — this is the admin-level cross-cutting view).

## Phase 5 — CRM Operational Controls 🔲 Not started

**Objective:** The spec's CRM-side requirements not already covered by Frappe CRM's existing integration.

- [ ] Confirm which of these Frappe CRM already provides natively (assignment via `ToDo`, notes) vs. what needs building — audit before implementing, don't duplicate.
- [ ] Notes/tags/workflow references specific to WhatsApp conversations, if not already covered by CRM's generic comment/tag system.
- [ ] Cross-reference with Phase 2's automation log — the spec's "automation logs showing why and when triggered" requirement spans both.

## Non-functional / production readiness

Moved into Phase 1.6 (RBAC audit, structured logging, deployment/process management — decided: Docker compose service, callback retry).

---

## How to use this doc

When starting a phase, flip its status to 🔄 In progress. When a task lands, check it off and note the commit/PR if useful. When a phase completes, flip to ✅ Done and add a one-line summary of what actually shipped (like Phase 1/1.5 above) rather than leaving the checklist as the only record.
