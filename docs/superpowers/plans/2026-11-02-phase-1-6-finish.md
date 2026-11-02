# Phase 1.6 Finish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish Phase 1.6: test safety, quoted replies, real reactions, health alerts, RBAC audit doc, media-limit doc, warm-up doc.

**Architecture:**
- Additive `/message/send` fields carry reply and reaction facts.
- The relay builds the WhatsApp `ContextInfo` and the reaction from its own session JIDs.
- Frappe pins reactions to the target's account.
- A new `alerts` module is called from the existing status helpers.
- Tests share a base class that blocks commits.

**Tech Stack:** Go 1.27 + whatsmeow (pinned), Frappe v16.

**Spec:** `docs/superpowers/specs/2026-11-02-phase-1-6-finish-design.md` (authority; §8 has the decisions). Earlier specs are in `docs/superpowers/specs/`.

## Global Constraints

- **Paths:** Go `yourwhatsapp/yourwhatsapp/relay/`; Frappe `yourwhatsapp/yourwhatsapp/`; tests `yourwhatsapp/yourwhatsapp/tests/` (all repo-relative).
- **Live site:**
  - Tests run on `whatsapp.localhost` (user-approved), but must never commit.
  - Never restart bench or the relay, touch `relay/data`, or send real WhatsApp messages.
  - Mock relay HTTP and `frappe.sendmail`.
- **Commits:** no Claude/AI trailer; no push.
- **No sticky sender**, except reactions (pinned to the target's `whatsapp_account`; refused at insert if that account isn't Active, and never failed over).
- **Additive `/message/send` fields:**
  - `reply_to_message_id` (string)
  - `reply_from_me` (bool)
  - `reply_text` (string; caption or `[image]`/`[video]`/`[audio]`/`[document]`)
- **Reactions:**
  - `content_type: "reaction"`;
  - `message` holds the emoji; an empty emoji removes the reaction;
  - `reply_to_message_id` and `reply_from_me` identify the target;
  - no anti-ban typing delay, and not counted toward `daily_message_count`.
- **JIDs:** a target from me uses the session's own JID; anything else uses the customer JID (`@lid` when `to_is_lid`, otherwise the phone JID).
- **Alerts:** `alerts.account_status_changed(account, old, new, reason)`.
  - Recipients: enabled users with the System Manager or WhatsApp Admin role.
  - Delivery: a bell notification (Notification Log) plus `frappe.sendmail`. With no outgoing email account, bell only, plus one Error Log entry per day.
  - Throttle: 15 min per account per kind (`down` / `recovered`). A recovery is sent only after a down alert, and the throttle never blocks it.
  - `reason="manual"` sends nothing and sets a 5 min manual flag that suppresses the relay's Disconnected that follows.
- **Running tests:**
  - Go: `export PATH=$HOME/.local/go/bin:$PATH; cd <relay> && CGO_ENABLED=1 go test -race ./... && CGO_ENABLED=0 go vet ./... && make build`.
  - Frappe: from `/workspace/development/onebook-bench`, `bench --site whatsapp.localhost run-tests --app yourwhatsapp --module <m>`; migrate only after a JSON or hooks change.
  - Don't run the full suite. The controller runs it with a before/after snapshot.

## Review Focus

1. **A reaction to an incoming message from a LID customer** builds its sender as `<lid>@lid`, not a phone JID (`TestReactionSenderLID`).
2. **A reply whose quoted row was received on a different number than the one sending** still sends, with the quote text intact (`test_reply_quote_across_numbers`).
3. **Flapping:** Disconnected → Active → Disconnected within 15 min produces one down alert, one recovery and no second down alert (`test_flap_throttled`).
4. **Admin Logout followed by the relay's Disconnected** produces no alert (`test_logout_then_relay_disconnect_silent`).
5. **Any test module not using the base class** makes the guard test fail (`test_all_modules_use_live_safe_base`).

---

### Task 1: Test safety base and review notes

**Files:**
- Create: `tests/utils.py` with `LiveSafeTestCase(FrappeTestCase)`. Its `setUp` patches `frappe.db.commit` (exposed as `self.commit_mock`) and rolls back in `tearDown`.
- Create: `tests/test_live_safety.py`. Guard test: for every `tests/test_*.py`, every `FrappeTestCase` subclass must subclass `LiveSafeTestCase`. Pure `unittest.TestCase` modules (e.g. `test_patches`) are allowed.
- Modify: every test module to use the base, removing per-module commit patches.
- Modify: `doctype/whatsapp_settings/whatsapp_settings.py`: `_keep_stored_keys` rebuilds the list (the caller's entries minus `STORED_KEYS`, then the kept ones).
- Modify: `relay_client.py`: the comment near the `handle_async_failure` row lock, plus the `adopt_pending_key` docstring, both noting that the commit also commits the dispatch's own message writes.
- Modify: `tests/test_default_accounts.py`: `test_existing_default_kept` makes B Active and asserts `(other, other)`; a new test, `test_inactive_default_repointed`, covers the repoint case.

**Interfaces:** Produces `tests.utils.LiveSafeTestCase`.

- [ ] Step 1: Failing tests:
  - `test_all_modules_use_live_safe_base` (fails today)
  - `test_keep_stored_keys_rebuilds_on_second_save` (same doc object saved twice, with a typed env-mode key on the second save → the key is stored)
  - `test_inactive_default_repointed`
- [ ] Step 2: Run, expect FAIL. Step 3: Implement. Step 4: Run each touched module, expect PASS.
- [ ] Step 5: Commit `test: live-safe base class for every test module; review notes`.

### Task 2: Relay replies and reactions

**Files:**
- Modify: `relay/internal/session` (`SendRequest` gains `ReplyToID`, `ReplyFromMe`, `ReplyText`; the send worker), `relay/internal/convert/outgoing.go` (`WithReply(msg *waE2E.Message, ctx *waE2E.ContextInfo)`, `BuildReaction` usage), `relay/internal/api/api.go` (decode the new fields), `cmd/relay/main.go` (startup log line with the effective `MEDIA_MAX_BYTES`), plus tests.

**Interfaces:**
- Produces:
  - `convert.ReplyContext(targetID, participantJID, quotedText string) *waE2E.ContextInfo`.
  - `convert.WithReply` sets `ContextInfo` on `ExtendedTextMessage` (text becomes extended text) and on Image/Video/Audio/Document messages.
- Reactions: `client.BuildReaction(chat, sender, targetID, emoji)` and send.
  - `sender` follows the JID rule.
  - Skip the typing delay and `daily_message_count`.
  - The idempotency `sends` row still applies.
- An empty or unknown `reply_to_message_id` sends a normal message.

- [ ] Step 1: Failing tests:
  - `TestReplyContextText`, `TestReplyContextMedia`
  - `TestReplyMissingTargetSendsPlain`
  - `TestReactionSenderFromMe`, `TestReactionSenderCustomer`, `TestReactionSenderLID`
  - `TestReactionEmptyEmojiRemoves`
  - `TestReactionSkipsDelay`
  - `TestSendDecodesReplyFields` (api)
  - `TestStartupLogsMediaCap`
- [ ] Step 2: FAIL. Step 3: Implement. Step 4: PASS with race, vet and build.
- [ ] Step 5: Commit `feat(relay): quoted replies and real reactions`.

### Task 3: Frappe replies and reactions

**Files:**
- Modify: `relay_client.py`: the payload adds `reply_to_message_id`, `reply_from_me` (target row `type == "Outgoing"`) and `reply_text` (target `message` or a media placeholder from its `content_type`), looked up by `reply_to_message_id` on WhatsApp Message.
- Modify: `doctype/whatsapp_message/whatsapp_message.py` `before_insert` for reactions:
  - pin `whatsapp_account` from the target;
  - raise `ValidationError("Reaction needs <account> to be connected")` if the target or account is missing or the account isn't Active;
  - skip the routing and template paths.
- Modify: `api.message_status`: don't increment `daily_message_count` for a reaction.
- Check CRM's inbound reply display:
  - read CRM's frontend and `get_whatsapp_messages` to see how `reply_to_message_id` / `is_reply` are rendered;
  - make sure inbound rows carry what CRM expects (e.g. it may look the quoted row up by `message_id`);
  - fix only on our side.
- Create: `tests/test_replies_reactions.py`.

**Interfaces:** Consumes the Task 2 payload contract.

- [ ] Step 1: Failing tests:
  - `test_reply_payload_from_incoming_target`, `test_reply_payload_from_outgoing_target`
  - `test_reply_media_target_placeholder`
  - `test_reply_quote_across_numbers`
  - `test_reply_missing_target_plain`
  - `test_reaction_pinned_to_target_account`
  - `test_reaction_inactive_account_refused_at_insert` (via `crm.api.whatsapp.react_on_whatsapp_message`, which raises)
  - `test_reaction_no_failover`
  - `test_reaction_not_counted`
  - `test_inbound_reply_visible_in_crm`
- [ ] Step 2: FAIL. Step 3: Implement. Step 4: PASS.
- [ ] Step 5: Commit `feat: quoted replies and reactions from CRM`.

### Task 4: Health alerts

**Files:**
- Create: `yourwhatsapp/yourwhatsapp/alerts.py`, `tests/test_alerts.py`.
- Modify: `routing.py` (`mark_disconnected`, `mark_rate_limited`, `set_account_active` call `alerts.account_status_changed` with the old status read before the write), `api.py` (`client_outdated` with reason), `doctype/whatsapp_account/whatsapp_account.py` (`pause` / `logout` pass `reason="manual"` and set the manual flag).

**Interfaces:**
- Produces `alerts.account_status_changed(account: str, old: str | None, new: str, reason: str | None = None) -> None`.
- Redis keys:
  - `yourwhatsapp:alert:<account>:down` (15 min)
  - `yourwhatsapp:alert:<account>:down_sent` (until recovery)
  - `yourwhatsapp:alert:<account>:manual` (5 min)
- Bell: `Notification Log` (type Alert, `document_type` WhatsApp Account).
- Email: `frappe.sendmail(recipients, subject, message, now=False)`. Skip email when no default outgoing Email Account exists, and log that once a day.

- [ ] Step 1: Failing tests:
  - `test_down_alert_bell_and_email`
  - `test_recipients_roles_enabled_only`
  - `test_flap_throttled`
  - `test_recovery_only_after_down`
  - `test_recovery_not_throttled`
  - `test_manual_pause_silent`
  - `test_logout_then_relay_disconnect_silent`
  - `test_rate_limit_expiry_is_recovery`
  - `test_client_outdated_reason_in_alert`
  - `test_no_email_account_bell_only`
  - Assert `sendmail` mocked.
- [ ] Step 2: FAIL. Step 3: Implement. Step 4: PASS.
- [ ] Step 5: Commit `feat: bell and email alerts when a WhatsApp number goes down or recovers`.

### Task 5: Docs (RBAC audit, media limits, warm-up)

**Files:**
- Create: `docs/rbac-audit.md` (matrix from the doctype JSONs plus whitelisted methods and role checks; CRM `ignore_permissions` inserts; risks; recommendations), `docs/warm-up.md`.
- Modify: `DEPLOYMENT.md` (media-limit chain: relay `MEDIA_MAX_BYTES` 15 MiB → about 20 MiB base64 → Frappe 25 MB request limit → `max_file_size` → nginx `CLIENT_MAX_BODY_SIZE` (verify the frappe_docker var and default from its docs or source; if unverifiable, say so); a link to warm-up), `ROADMAP.md` (tick the §C RBAC audit as "audited, see docs/rbac-audit.md", the §C nginx item, the §E reactions and replies, the §F warm-up, the §G health alert; flip Phase 1.6 to ✅ only if nothing is left open), `CLAUDE.md` (reaction exception to no-sticky; the alerts module; the test base class rule).

- [ ] Step 1: Write the docs. Never say "ban-proof".
- [ ] Step 2: Commit `docs: RBAC audit, media limits, warm-up policy; Phase 1.6 complete`.

### Task 6 (controller + user): live check

- [ ] Snapshot live state.
- [ ] After the user restarts bench, check `/info` shows 2/2.
- [ ] Send Monica → Primary "[reply test]"; then from Primary, a quoted reply to it.
- [ ] React 👍 to the reply.
- [ ] Ask the user to confirm on the phone that the quote and the reaction display.
- [ ] Trigger an alert safely: call `alerts.account_status_changed` for Sharaj (already Disconnected, not linked) with old Active and new Disconnected. Check that the bell (and email if configured) arrives, then clean up the Notification Log row if the user wants.
- [ ] Diff the snapshot.
