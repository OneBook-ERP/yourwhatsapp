# Design: Finish Phase 1.6: replies, reactions, health alerts, RBAC audit, media limit, warm-up, test safety

- **Date:** 2026-11-02
- **Status:** Draft, awaiting approval
- **Roadmap:** Phase 1.6 §C (RBAC audit, nginx body size), §E (reactions, replies), §F (warm-up policy), §G (health alert), plus review cleanup. Once this ships, Phase 1.6 is complete and Phase 2 (trigger messaging) is next.
- **Builds on:** `2026-10-31-whatsmeow-relay-design.md`, `2026-10-31-phase-1-6-blockers-design.md`. The client requirement in `CLAUDE.md` still holds: no sticky sender, and threads are keyed by customer.

## 1. Intent

These are the last items before go-live:
- CRM replies should look like WhatsApp replies (quoted).
- CRM reactions should be real reactions, not a text message.
- Staff should learn that a number dropped from a notification, not from failed sends.
- The team needs a clear record of who can do what.
- Production must accept large inbound media.
- New numbers need a documented warm-up so they aren't banned in week one.

Tests run on the live site (user's choice), so the suite itself must never be able to commit.

## 2. Goals

0. **Test safety + review cleanup**
   - A shared test base class patches `frappe.db.commit` for every test module, and every module uses it.
   - A guard test fails if any test module does not use the base.
   - Three review notes:
     - `_keep_stored_keys` rebuilds `ignore_save_passwords` from scratch on each save.
     - Document that `adopt_pending_key`'s commit also commits the dispatch's own message writes, in the `relay_client` comment and the docstring.
     - `test_existing_default_kept` asserts what its name says; the repoint case moves to its own correctly named test.
1. **Quoted replies**
   - **Outgoing:** when a WhatsApp Message has `reply_to_message_id`, the relay sends it as a WhatsApp reply: `ContextInfo` with `StanzaID`, `Participant` and `QuotedMessage` (text), for both text and media sends. Frappe passes the quoted message's id, the participant's number (whose message is quoted), and the quoted text, looked up from the referenced WhatsApp Message.
   - **Rotation is unchanged.** A quote carries its own text, so it shows correctly even when the reply goes out from a different number. If the quoted message can't be found in Frappe, the message is sent as a normal message, not failed.
   - **Incoming:** the existing `is_reply` / `reply_to_message_id` reach CRM. Verify CRM's chat shows the reply context, and fix whatever stops it.
2. **Real reactions**
   - `content_type: reaction`: the relay sends `BuildReaction(chat, sender, target_id, emoji)`, where `sender` is the customer for an incoming target and our own number for an outgoing target. An empty emoji removes the reaction.
   - **Exception to rotation, technically required:** a reaction must come from the number whose chat holds the target message, which is the account on that WhatsApp Message. If that account isn't Active, the reaction fails at once with "Reaction needs <account> to be connected". It does not fail over to another number.
   - Reactions don't count toward `daily_message_count` and skip the anti-ban typing delay. The relay still queues them per account.
3. **Health alerts**
   - **Trigger:** an account moves to Disconnected, RateLimited, or the relay reports `client_outdated`.
   - **Who gets it:** every enabled user with System Manager or WhatsApp Admin.
   - **How:** a Frappe **bell notification** (Notification Log) plus an **email**.
   - **Content:** account name and number, new status, reason (relay detail, ban expiry, or "client outdated — upgrade relay"), time, and a link to the account.
   - **Recovery:** when the account returns to Active, a "back online" bell + email.
   - **Dedupe:** alert only on a real transition, at most one alert per account per 15 min (Redis key), so flapping doesn't spam.
   - **No email account configured:** bell only, plus one Error Log note per day.
   - A manual Pause or Logout by an admin does **not** alert.
4. **RBAC audit (document only, no permission changes)**
   - New `docs/rbac-audit.md`, covering:
     - a matrix of every role × doctype × right for WhatsApp Message / Account / Settings / Templates / Template Parameter;
     - which whitelisted methods each role can call (`connect`, `pair_with_code`, `pause`, `resume`, `logout`, the Settings methods, the `api.*` callbacks);
     - CRM's `ignore_permissions` inserts;
     - concrete risks, for example Sales User can write WhatsApp Message directly over REST and bypass CRM's checks;
     - recommendations to apply later.
   - Tick the roadmap item as "audited".
5. **Inbound media size in production**
   - `DEPLOYMENT.md` gives the chain of limits:
     - relay `MEDIA_MAX_BYTES` (15 MiB) becomes about 20 MiB once base64-encoded, plus JSON;
     - Frappe's request limit (25 MB);
     - Frappe `max_file_size` (System Settings);
     - frappe_docker nginx `CLIENT_MAX_BODY_SIZE` (verify the default and the env var name from the frappe_docker source or docs; if unverifiable, say so and give the setting to check).
   - Explain how to raise them together.
   - Add a relay startup log line with the effective media cap.
6. **Warm-up policy:** a `docs/warm-up.md` section (linked from DEPLOYMENT), covering:
   - new numbers start at `daily_limit` 20;
   - roughly double every 3–4 days up to the target over 1–2 weeks;
   - real SIM numbers only;
   - mix inbound conversations early;
   - avoid identical bulk text;
   - what a `TemporaryBan` alert means and what to do.

   This is risk reduction only, never "ban-proof". There are no code changes; the defaults stay.

## 3. Non-goals

- Changing permissions (audit only, by the user's decision).
- Phase 2 triggers, the Phase 3 health score, and the Phase 4 dashboard.
- Reactions or replies from the phone itself mirrored into CRM, beyond the existing inbound handling.
- SMS or other alert channels.

## 4. Anti-goals

- A reaction sent from the wrong number, which would be silently ignored. It must fail visibly instead.
- Alert spam from flapping connections.
- Any test, new or old, that commits to the live site.
- Weakening the no-sticky-sender rule for anything except reactions.

## 5. Constraints

- Same Go and Frappe conventions as before.
- Live system rules:
  - Tests run on `whatsapp.localhost` (user-approved), with the controller snapshotting before and diffing after.
  - No real sends except between the user's own linked numbers.
  - No bench or relay restarts by agents.
- No Claude/AI trailer in commits, and no push.

## 6. Testing

- **Go:**
  - Reply `ContextInfo` on text and media.
  - Reaction build: incoming vs outgoing target, empty emoji, skips the delay, not counted.
  - Bad or missing quoted fields are ignored gracefully.
- **Frappe:**
  - Reply payload fields from the referenced message, and a missing reference sends as normal.
  - Reaction pinned to the target's account; inactive account fails clearly; no failover.
  - Alerts:
    - recipients;
    - bell + email (mock sendmail);
    - dedupe within 15 min;
    - recovery notice;
    - no alert on manual Pause or Logout;
    - no email account → bell only.
  - Base-class guard test.
- **Live** (controller, own numbers only):
  1. Send a quoted reply Monica → Primary.
  2. React 👍 to it.
  3. Check that both show correctly in WhatsApp; the user confirms on the phone.

## 7. Left to the builder

- Notification Log subject and email wording.
- Redis key naming.
- Where in the routing helpers the alert hook sits, as long as there is one hook per status transition.
- Structure of `docs/rbac-audit.md`.

## 8. Decisions (from builder check, controller calls)

1. **Reaction feedback:** WhatsApp Message `before_insert`, when `content_type == "reaction"`:
   - Find the target by `reply_to_message_id` and pin `whatsapp_account` to the target's account. This bypasses rotation.
   - If the target or its account is missing, or the account isn't Active, raise `frappe.ValidationError("Reaction needs <account> to be connected")`. CRM's `react_on_whatsapp_message` then shows the error to the agent.
   - Failures that only happen later, during the async send, mark the row Failed and never fail over.
2. **Account field:** `whatsapp_account` is set on every inbound row (relay session) and every outbound row (routing), so it is the source of truth. A target without it → the reaction is refused.
3. **JIDs:** Frappe sends only facts: `reply_from_me` (bool, true when the target row is Outgoing) and the existing `to` / `to_is_lid`. The relay builds the JIDs from its own session:
   - from-me target → the session's own JID;
   - otherwise → the customer JID (`@lid` when `to_is_lid`, else the phone number JID).

   The same rule applies to the quote `Participant` and the reaction `sender`.
4. **Payload contract (additive, no version gating; relay and app ship together):** `/message/send` gains optional `reply_to_message_id`, `reply_from_me`, `reply_text`.
   - For a media target, `reply_text` = its caption, or a placeholder such as `[image]`.
   - Reactions: `content_type: "reaction"`, `message` = emoji (empty removes it), `reply_to_message_id` + `reply_from_me` identify the target.
   - An unknown or missing target id on a reply → send as a normal message.
5. **Alert transitions:** one function, `alerts.account_status_changed(account, old, new, reason)`, is called from the routing helpers (`mark_disconnected`, `mark_rate_limited`, `set_account_active`) and the `client_outdated` handler. It fires only when `old != new` and one side is Active.
   - Manual Pause/Logout pass `reason="manual"` → no alert, and set a 5-minute Redis "manual" flag so a later relay Disconnected for that account doesn't alert either.
   - A RateLimited account returning to Active on ban expiry counts as "back online".
   - Recipients: users with `enabled=1` and a Has Role row for System Manager or WhatsApp Admin, excluding Guest; Administrator is included only if it has an email.
6. **Throttle:** a 15-minute throttle per account per alert kind (down / recovered). A recovery is sent only if a down alert was sent for that outage, and the throttle never blocks a recovery.
7. **`_keep_stored_keys`:** add a test for a doc object saved twice, with a typed env-mode key on the second save.
