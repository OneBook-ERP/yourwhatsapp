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

## Phase 2 — Trigger-Based Automated Messaging 🔲 Not started (chosen next priority)

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

## Non-functional / production readiness (cross-cutting, not a single phase)

- [ ] RBAC audit: `WhatsApp Admin`/`WhatsApp Operator` roles exist on doctypes but haven't been reviewed end-to-end against the spec's "admins, operators, and sales/support users" requirement.
- [ ] Structured logging for debugging/audit purposes beyond the current ad-hoc `console.error` calls in the relay.
- [ ] Deployment docs for the relay as a properly-managed process (systemd/pm2), not just `bench start`'s Procfile entry — production process management was explicitly called out as a deployment-time decision in the Phase 1 plan, still undecided.
- [ ] Reliable webhook/callback handling review — current retry behavior on relay↔Frappe callback failures is minimal (log-and-drop in `frappe_client.js`).

---

## How to use this doc

When starting a phase, flip its status to 🔄 In progress. When a task lands, check it off and note the commit/PR if useful. When a phase completes, flip to ✅ Done and add a one-line summary of what actually shipped (like Phase 1/1.5 above) rather than leaving the checklist as the only record.
