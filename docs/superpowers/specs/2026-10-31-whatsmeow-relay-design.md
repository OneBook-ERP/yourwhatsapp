# Design: Replace the whatsapp-web.js relay with a whatsmeow (Go) relay

- **Date:** 2026-10-31
- **Status:** Draft, awaiting approval
- **Roadmap:** Phase 1.6, new section 0 ("Relay engine migration"), see `ROADMAP.md`

## 1. Intent

`yourwhatsapp` is an internal WhatsApp sending and receiving layer for a cannabis-industry CRM. Meta's official API is off the table (restricted category), so the relay runs an unofficial client.

Today that client is `whatsapp-web.js`, which drives headless Chromium running WhatsApp Web. In production that has cost us:

- **Breakage:** WhatsApp's July 2026 Web update broke message ids, media download and delivery receipts (`_serialized` → `$1`).
- **Unreliable startup:** the `ready` event hangs. We poll page state and inject listeners by hand, and getting that timing wrong corrupts sessions.
- **Heavy and messy to run:** 300–500 MB of Chromium per number, orphaned browsers holding profile locks, no native ARM build, and a hardcoded `Chrome/101` user-agent shared by every install.
- **Docker friction:** most of the Phase 1.6 Docker and session work exists only because there's a browser.

**whatsmeow** (`go.mau.fi/whatsmeow`) speaks WhatsApp's multi-device protocol directly over a websocket, with no browser. It's actively maintained because it powers the mautrix-whatsapp bridge. A comparison on 2026-10-31 scored it 4.20 out of 5, against 3.95 for Baileys, 3.20 for Evolution API and 2.70 for staying on whatsapp-web.js. The decision is made: **migrate the relay to whatsmeow.**

**Why now:** Phase 1.6 (production readiness) is about to build Docker images, session restore, graceful shutdown and callback retry around the browser-based relay. Doing that on the old engine would mean building it twice.

## 2. Goals

1. A Go relay replaces `yourwhatsapp/yourwhatsapp/relay/` (Node). It has the same HTTP API, so Frappe needs only small changes.
2. **Pairing:** QR code (as today) plus a new **phone-number pairing code**.
3. **Sessions persist** in one SQLite file on a volume. Every paired account reconnects automatically when the relay boots, with no rescan.
4. **Sending:** text and media (image, video, audio, document, with caption); check the number is on WhatsApp before sending; normalize phone numbers; the message id is returned with the send result.
5. **Receiving:** one-to-one chats only; media downloaded; LID senders resolved to a phone number where possible, otherwise stored raw and flagged; quoted-reply context passed through.
6. **Delivery and read receipts** sent to Frappe; status only moves forward (`Sent` → `Delivered` → `Read`).
7. **Relay-to-Frappe callbacks never get lost:** a persistent outbox with retry and backoff.
8. **Safe failover:** relay errors are classified so Frappe fails over, retries, or fails permanently correctly, and a retried send is never delivered twice.
9. **Minimum anti-ban:** a send queue per account with randomized delay and a typing indicator; optional proxy per account; realistic device identity; a temporary-ban event pauses the account and brings it back automatically.
10. **Clean shutdown on SIGTERM:** every client disconnects before the process exits.
11. **Multi-account:** one process, one client per `WhatsApp Account`; adding a number needs no restart.
12. A relay `Dockerfile` (multi-stage, static binary, `/data` volume).

## 3. Non-goals (this spec)

- **Relay claim flow** (setting the API key from the UI) and the settings-page relay panel. Still Phase 1.6 section A, built separately on top of this relay.
- Docker compose wiring in the production stack and `DEPLOYMENT.md`.
- Phase 2 trigger-based messaging and Phase 3 health scoring, beyond the `TemporaryBan` hook in goal 9.
- Groups, status updates, channels, broadcast lists, calls, polls, stickers, location (inbound is dropped; outbound is unsupported).
- Message reactions and outgoing quoted replies. Section 8 covers the protocol support, but the CRM-side wiring stays a separate Phase 1.6 E item. Inbound `reply_to_message_id` **is** in scope (goal 5).
- Migrating existing whatsapp-web.js sessions. That isn't possible: every number must be relinked once.

## 4. Anti-goals: failure even if it "works"

- **Calling this ban-proof or Meta-approved anywhere** (code, docs, UI). It's risk reduction only (compliance requirement).
- Anything that makes Frappe depend on whatsmeow's types or the `@s.whatsapp.net` format. The relay translates; Frappe keeps seeing digits-only numbers and opaque message ids.
- Sending the same message twice because of failover or retry.
- Losing an inbound message because Frappe was restarting.
- Any public (unauthenticated) endpoint that returns data, or a QR or pairing code reachable without the API key.
- Keeping both engines running side by side. The Node relay is deleted once the Go relay is verified.

## 5. Constraints

- **Host:** ARM64 (`aarch64`) dev container. Go isn't installed; install a current Go toolchain into the user's home directory (no root). Production is Docker with **no shell access** for us.
- **No CGO:** use a pure-Go SQLite driver (`modernc.org/sqlite`) so the binary is static and cross-compiles.
- `.localhost` names don't resolve from the relay process; callbacks use `http://127.0.0.1:<port>` (env `FRAPPE_BASE_URL`, unchanged).
- Env vars stay: `RELAY_API_KEY`, `FRAPPE_BASE_URL`, `RELAY_PORT` (not `PORT`). New: `RELAY_DATA_DIR` (default `./data`), `RELAY_HOST` (default `127.0.0.1`; `0.0.0.0` in Docker).
- Request body limit is 25 MB (base64 media).
- Frappe-side code follows the existing app conventions: tabs, `frappe.db.set_value`, private files.
- **Pin the exact whatsmeow version** in `go.mod`. Upgrades are deliberate.
- The bench `Procfile` `relay:` entry must run the new binary, so `bench start` still brings everything up.

## 6. Architecture

```
Frappe (unchanged seam)                         Go relay  (yourwhatsapp/yourwhatsapp/relay/)
───────────────────────                         ─────────────────────────────────────────────
relay_client.py ──HTTP+x-api-key──>  api/        HTTP handlers, auth, error codes
api.py  <──HTTP+X-Api-Key (outbox)── manager/    map[session_id]*Session, boot restore, shutdown
                                     session/    one whatsmeow.Client + send queue + event handler
                                     outbox/     persistent callback queue + retry worker
                                     store       data/relay.db  (SQLite: whatsmeow tables
                                                  + our tables: accounts, outbox, sent_ids)
```

A single SQLite database `data/relay.db` holds both whatsmeow's `sqlstore` tables and ours:

- **`accounts`**: `session_id` (PK), `jid` (nullable until paired), `proxy` (nullable), `paused` (bool), `created_at`.
- **`outbox`**: `id`, `method` (`inbound_message` / `message_status` / `update_qr` / `account_event`), `payload` (JSON), `attempts`, `next_attempt_at`, `last_error`, `created_at`.
- **`sends`**: `local_message_id` (PK, Frappe's `WhatsApp Message.name`), `session_id`, `state` (`queued` / `sent` / `failed`), `message_id` (nullable), `error_code`, `created_at`, `updated_at`. This is the idempotency record, written **when the send is accepted**, not after it goes out.
- **`outbox_dead`**: same columns as `outbox` plus `dead_at`. Rows go here instead of being deleted, so nothing is silently lost.

## 7. HTTP API (relay)

Every route except `/health` needs `x-api-key`, compared in constant time. All responses are JSON.

| Method & path | Body / response | Notes |
|---|---|---|
| `GET /health` | `{"ok":true}` | No auth, no data. Used by the Docker healthcheck. |
| `POST /session/:id/start` | `{"proxy": ...}` (optional) → `{"status": ...}` | Creates the account row if new. If paired, connects. If not paired, starts the QR flow. Idempotent: calling it on a running session returns its status. **Must work after a disconnect or logout** (fixes the stale-map bug). |
| `POST /session/:id/pair-code` | `{"phone":"+91 98765 43210"}` → `{"code":"ABCD-1234"}` | Phone-number pairing. Session must be unpaired. |
| `GET /session/:id/status` | → `{"status", "jid", "phone"}` | |
| `POST /session/:id/pause` | | Disconnects the client and sets `paused=1`; not restored on boot. |
| `POST /session/:id/resume` | `{"proxy": ...}` (optional) | Sets `paused=0` and connects. |
| `POST /session/:id/logout` | | Unlinks the device from WhatsApp, deletes the device store, keeps the account row with `jid=NULL`. |
| `POST /message/send` | Same body as today (`session_id`, `to`, `content_type`, `message`, `local_message_id` (now **required**), `attach_base64`, `attach_mimetype`, `attach_filename`) plus optional `to_is_lid` (bool) → **202** `{"state":"queued"}` | **Asynchronous.** Section 8.3. The final result (`Sent` with `message_id`, or `Failed` with an error code) arrives later through the `message_status` callback. |

`status` values (relay-internal): `not_started`, `qr_pending`, `pairing`, `connecting`, `connected`, `disconnected`, `logged_out`, `paused`, `temp_banned`.

**Error contract for `/message/send`.** Errors are found either **synchronously** (in the HTTP response) or **asynchronously** (later, in a `message_status(local_message_id, status="Failed", error_code=...)` callback). Both use the same codes; the body is `{"error": "<code>", "detail": "..."}`:

| HTTP (sync) | `error` | Meaning | What Frappe does |
|---|---|---|---|
| 400 | `bad_request` | Missing fields or bad base64 | `Failed`, no failover |
| 422 | `invalid_number` | Number not on WhatsApp, or unparseable | `Failed`, no failover |
| 409 | `session_unavailable` | Not connected, paused, logged out, or queue full | Fail over to the next account |
| 429 | `temp_banned` | WhatsApp temporary ban (`detail` includes the expiry) | Account → `RateLimited`, then fail over |
| — (async only) | `send_failed` | whatsmeow returned an error while sending | Fail over (enqueued job) |
| 202 | — | Accepted and queued | `Queued` stays; wait for the callback |
| 200 | `duplicate` | `local_message_id` is already `queued` or `sent` **on any session**; `state` and `message_id` are returned and nothing is queued | No-op |

A `local_message_id` whose earlier attempt ended `failed` can be accepted again (on any session): that's how failover reuses the same message.

## 8. Behaviour

### 8.1 Pairing
- **QR:** call `GetQRChannel` before `Connect`. Each `code` event is rendered to a PNG (`github.com/skip2/go-qrcode`), base64-encoded, and sent through the outbox as `update_qr(session_id, qr_base64)`. `success` → status `connected`, store the JID, `update_qr(status="Active")`. `timeout` → status `disconnected`, `update_qr(status="Disconnected")`.
- **Pairing code:** connect first, then call `PairPhone(phone, showPushNotification=true, PairClientChrome, "Chrome (Linux)")`. Return the code. Completion arrives through the same `PairSuccess` / `Connected` events.

### 8.2 Lifecycle and restore
- **On boot:** for every `accounts` row with a `jid` and `paused=0`, load the device from the store and `Connect()` (auto-reconnect on). Restored sessions then report `Active` through the outbox.
- **Events → Frappe**, all through `update_qr(session_id, status=...)` so `api.py` needs no new endpoint for status:
  - `Connected` → `Active`
  - `Disconnected` → no change (auto-reconnect handles it; avoids status flapping)
  - `LoggedOut` → delete the device store, set `jid=NULL`, send `Disconnected`
  - `StreamReplaced` → disconnect without reconnecting, send `Disconnected`, log it loudly
  - `TemporaryBan` → status `temp_banned`; send the new `account_event` callback (section 9) with `RateLimited` plus the expiry. When the expiry passes the relay reconnects, and `Connected` sends `Active` again.
- **Who owns account status:** Frappe owns admin intent; the relay owns connection facts.
  - The relay never resumes a session that Frappe paused: pause and resume always go Frappe → relay.
  - In Frappe, a relay `Active` / `Disconnected` event is **ignored when the account is `Paused`**.
  - A relay `Active` event clears `RateLimited` only if `rate_limited_until` is set and has passed (relay-originated ban). A manually set `RateLimited` (no `rate_limited_until`) stays until an admin changes it.
- **Proxy:** Frappe's `WhatsApp Account.proxy` is the source of truth. It's sent with every `start` / `resume` (as JSON `{"proxy": ...}`) and the relay stores a copy only so it can restore sessions on boot.
- **On SIGTERM/SIGINT:** stop accepting HTTP, then for each client stop its send queue, wait up to 10 s for any in-flight send, call `Disconnect()`, then exit. Target: finished within 25 s (compose `stop_grace_period: 30s`).

### 8.3 Sending
The HTTP call returns as soon as the message is accepted. The Frappe worker never waits for the anti-ban delay, so a single `bench worker` isn't blocked for minutes.

**Synchronous part (in the request):**
1. **Idempotency:** look up `sends` by `local_message_id`. If it's `queued` or `sent` on any session, return 200 `duplicate`.
2. **Session check:** the session must be `connected` → otherwise `session_unavailable`. If it's `temp_banned` → `temp_banned`.
3. **Normalize `to`:** strip everything except digits.
   - If `to_is_lid` is set, the target is `<digits>@lid` and the number check is skipped.
   - Otherwise reject fewer than 8 or more than 15 digits (`invalid_number`).
4. **Check the number** (not for LIDs): `IsOnWhatsApp(["+"+digits])`. If it isn't registered → `invalid_number`. Use the returned JID as the target. Cache positive results for 24 h in memory.
5. **Queue capacity:** if the account's queue already holds 50 items → `session_unavailable`.
6. **Reserve:** insert or overwrite the `sends` row as `queued` for this session, **then** enqueue the job, then return 202. The media bytes stay in memory with the job. A relay restart loses queued jobs; on boot, every `sends` row still `queued` is marked `failed` with `send_failed`, and a `message_status` Failed callback goes out, so Frappe fails over.

**Asynchronous part (per-account worker):**
4. **Queue:** each account has its own FIFO queue and worker goroutine. Before each send, wait a random time in `[SEND_DELAY_MIN, SEND_DELAY_MAX]` since that account's previous send (env, defaults 4 s and 12 s; `0/0` allowed for dev). If the session isn't connected when a job comes up, mark it `failed` with `session_unavailable`; if the session is in a temporary ban, mark it `failed` with `temp_banned`.
5. **Typing indicator:** send `composing` presence, wait 1–3 s depending on text length, then `paused`.
6. **Send:**
   - Text: `Conversation`.
   - Media: `Upload` (type from `content_type`), then `ImageMessage` / `VideoMessage` / `AudioMessage` / `DocumentMessage` with caption (document: file name plus caption). An unknown `content_type` with an attachment is sent as a document.
   - Use the client's own message-id generator.
7. **On success:** set `sends.state=sent` and `message_id`, then send the outbox callback `message_status(local_message_id, message_id, status="Sent")`. **On error:** set `failed` with `send_failed`, then send `message_status(local_message_id, status="Failed", error_code, error_reason)`. The `Sent` callback always goes into the outbox **before** any receipt for that id can arrive, because both go through the same ordered outbox.

### 8.4 Receiving
- Handle `*events.Message`. Drop the message if any of these is true: `IsFromMe`; chat server not in {`s.whatsapp.net`, `lid`}; it's a protocol, reaction or edit message with no user content. Reactions inbound are dropped in this spec (Phase 1.6 E).
- **Sender phone number:** if the chat JID is `@lid`, look it up in the store's LID map. If it resolves, use those digits. If not, send `from_number` as the raw LID digits **and** a new `from_lid=true` flag.
- **Replies to unresolved LIDs:** in Frappe, `relay_client` sets `to_is_lid=true` when the latest incoming `WhatsApp Message` whose `from` equals `to` has `is_lid_sender=1`, so the relay sends straight to `<digits>@lid`. That avoids treating LID digits as a phone number (rejected, or worse, matched to a real, unrelated number). CRM still links on `from`; an unresolved LID won't match a Lead, which is accepted. Staff see the flag.
- **Content type:** text (`Conversation` / `ExtendedTextMessage`), `image`, `video`, `audio`, `document`. Others become text with a short placeholder (e.g. `[sticker]`) so nothing disappears silently.
- **Media:** `Download` the content, base64-encode it, and include mimetype and file name (documents use their file name; others are named `<message_id>.<ext>`). If the download fails, the message is still delivered without media and the error is logged.
- **Replies:** if `ContextInfo.StanzaID` is set, send `is_reply=true` and `reply_to_message_id`.
- Send to the outbox as `inbound_message` with the same fields as today, plus `from_lid`.

### 8.5 Receipts
- Handle `*events.Receipt` for one-to-one chats only. `Delivered` → `Delivered`, `Read` / `ReadSelf` → `Read`, `Played` → `Read`. Send one `message_status` per id.
- **Frappe makes status monotonic** (section 9). The relay just reports.

### 8.6 Outbox
- Every callback to Frappe goes through the outbox table. A single worker sends due rows **one at a time in insertion order**, so status updates for a message can't overtake the inbound message they belong to.
- **Retries:** on network error or 5xx, retry with exponential backoff (1 s, 2 s, … capped at 5 min) without giving up.
- **Permanent errors:** on 4xx (except 429), log, drop the row, and record the drop in the log, so one bad row can't block the queue forever.
- Delete a row on 2xx.
- **Response classes from Frappe:**
  - **401/403** (key mismatch): **pause the whole outbox**. Retry the head row every 60 s forever and log an error each time. Never drop.
  - **404** (unknown session or account): retry with backoff for up to 24 h, then move the row to `outbox_dead`.
  - **Other 4xx** (bad payload): move to `outbox_dead` immediately.
- Rows are never deleted except on 2xx.
- `GET /outbox/dead` (authenticated) lists dead rows, for debugging.
- Callbacks set a `Host` header from optional env `FRAPPE_SITE_NAME`, which is ready for multi-site benches.

### 8.7 Device identity and proxy
- Set `store.DeviceProps` to OS `"Linux"` with platform type `CHROME` (as if a desktop browser linked). Pairing uses the `PairClientChrome` client type.
- If `accounts.proxy` is set, call `client.SetProxyAddress(proxy)` before `Connect`.

### 8.8 Logging
- Structured JSON to stdout (`log/slog`), with `session_id` on every per-account line. Never log message bodies, media, QR codes or API keys.

## 9. Frappe-side changes

- **`relay_client.py`:**
  - `TIMEOUT` stays short (30 s; the call only queues).
  - Always send `local_message_id` and `to_is_lid`. On 202, leave the status `Queued`. On 200 `duplicate`, do nothing.
  - Branch on the **synchronous** error codes in section 7: `invalid_number` / `bad_request` → `Failed`, no failover; `temp_banned` → account `RateLimited`, then fail over; `session_unavailable` / connection error → fail over (existing path).
  - **Timeout** (unknown whether it was accepted): retry once on the **same** account. Idempotency returns `duplicate` if the first call did land.
  - **Asynchronous failures:** `message_status` with `status="Failed"` and `error_code` in {`session_unavailable`, `send_failed`, `temp_banned`} enqueues a failover dispatch, excluding accounts already tried. Store the tried accounts in a new hidden field `attempted_accounts` (Small Text, JSON list) on `WhatsApp Message`. `invalid_number` → just `Failed`.
  - **`daily_message_count`:** incremented on the `Sent` callback, not on 202, using an atomic `UPDATE ... SET daily_message_count = daily_message_count + 1`.
- **`api.py`:**
  - `message_status(message_id=None, status, error_reason=None, local_message_id=None, error_code=None)`:
    - Look up by `local_message_id` (the `name`) if given, otherwise by `message_id`. On `Sent`, store `message_id`.
    - **Monotonic:** rank `Queued`=0, `Sent`=1, `Delivered`=2, `Read`=3 and ignore a lower rank. `Failed` applies only while status < `Sent`, and triggers the failover handling above.
    - Unknown ids stay a no-op.
  - `inbound_message`:
    - Accept `from_lid: bool`. When it's true, also set the new `WhatsApp Message.is_lid_sender` field so staff can see the number is unresolved.
    - **Dedupe:** if a `WhatsApp Message` with the same `message_id` and `type=Incoming` already exists, return the existing name.
  - **New** `account_event(session_id, event, until=None)`: `event="temp_banned"` sets the account to `RateLimited` and stores `rate_limited_until`.
  - `update_qr`: unchanged.
- **Doctype changes** (with a migration patch where needed):
  - `WhatsApp Message`: new Check `is_lid_sender` (read-only); new hidden Small Text `attempted_accounts`.
  - `WhatsApp Account`:
    - New Data `proxy`, sent to the relay on connect.
    - New Datetime `rate_limited_until` (read-only).
    - New Select `pairing_method`: `QR Code` | `Phone Number`, default QR Code.
  - **Account form buttons:**
    - **Connect** follows `pairing_method`. QR as today; Phone Number calls the new `pair_with_code()` method and shows the 8-character code in a dialog with instructions (Linked Devices → Link with phone number).
    - **Pause / Resume** and **Logout** call the matching relay routes, same role check as `connect()`.
- **Scheduler (every 5 min):** accounts in `RateLimited` whose `rate_limited_until` has passed go back to `Active`.
- **Procfile:** `relay:` runs the Go binary (`apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay/bin/relay`), built with `make build` in `relay/`.

## 10. Cutover

1. Build and run the Go relay on port 8800 in place of Node (stop Node first; kill its Chromium children too, see the `CLAUDE.md` gotcha).
2. Every existing `WhatsApp Account` must be relinked once (unlink the old "Chrome" device on the phone first, to free a linked-device slot).
3. Once send, receive, media, receipts, restart-restore and failover are verified against real numbers, delete the Node relay (`server.js`, `session_manager.js`, `frappe_client.js`, `package*.json`, `node_modules`, `.sessions/`).

## 11. Testing

- **Go unit tests:** number normalization; error-code mapping; outbox ordering, backoff and permanent-drop; idempotency (`sent`); the send queue's delay bounds and capacity; the inbound filter (server/type matrix); `content_type` mapping; LID fallback. Event handling is tested with whatsmeow types built directly, so no network is needed.
- **Go HTTP tests:** auth (missing or wrong key → 401, `/health` open), every route, error contract.
- **Frappe tests** (`bench run-tests --app yourwhatsapp`):
  - `message_status` monotonicity.
  - `inbound_message` dedupe and `from_lid`.
  - `relay_client` branching (mock `requests`): invalid number → no failover; 409 → failover; timeout → same-account retry; 429 → `RateLimited`; asynchronous `Failed` callback → failover job with `attempted_accounts`; `Sent` callback → atomic count increment; a `Paused` account ignores relay `Active`.
  - `account_event`.
  - The `RateLimited` recovery job.
- **Manual verification against real numbers** (user scans): QR pairing; pairing-code pairing; text send and receive; media send and receive (image, PDF, voice note); delivery and read receipts; relay restart → sessions restore without rescanning; Frappe stopped during inbound → message arrives after Frappe returns; failover between two numbers; logout from the phone → account shows Disconnected.

## 12. Left to the builder

- Go package layout inside `relay/` (sketch in section 6 is guidance), HTTP router (stdlib `net/http` with Go 1.22+ patterns is fine), and test helpers.
- Exact backoff jitter, the TTL of the number-check cache, and the placeholder strings for unsupported inbound types.
- Exact whatsmeow version to pin: latest at build time, recorded in `go.mod` and noted in `CLAUDE.md`.
- `Makefile` targets (`build`, `test`, `run`).
- `Dockerfile` details beyond: multi-stage, `CGO_ENABLED=0`, distroless or alpine runtime, non-root, `VOLUME /data`, `HEALTHCHECK` on `/health`. Not verifiable on this host (no Docker); note that in the PR.

## 13. Calls made by Claude (not explicitly decided by the user)

- One SQLite file for both whatsmeow's store and the relay's own tables; pure-Go driver.
- The send queue's default delay of 4–12 s, overridable by env.
- Send status values reuse the existing `update_qr` callback; only temp-ban gets a new `account_event` callback.
- Status changes on `Disconnected` are suppressed (auto-reconnect), to avoid flapping.
- Timeout retries the same account once before failing over.
- **Asynchronous send** (202 plus a callback), instead of a blocking send. The reason: a single `bench worker` and the job timeout can't absorb anti-ban delays. This changes the `/message/send` contract and `message_status` (adds `local_message_id` and `error_code`).
- `sends` is reserved before queueing and deduped across sessions; jobs still queued at relay boot are failed so Frappe fails them over.
- Outbox: 401/403 pause and retry forever; 404 retries for 24 h; other 4xx go to `outbox_dead`. Nothing is silently dropped.
- Unresolved LID senders keep `from` = LID digits plus `is_lid_sender`; replies use `to_is_lid` so they're sent to `@lid`, not number-checked.
- Status ownership: Frappe owns Pause and manual RateLimited; the relay owns connection facts; Frappe's proxy field is the source of truth.
- Inbound reactions dropped for now.
