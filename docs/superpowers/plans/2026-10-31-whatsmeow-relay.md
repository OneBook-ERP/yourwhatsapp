# whatsmeow Relay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the Node/whatsapp-web.js relay with a Go relay built on whatsmeow, plus the Frappe-side changes the new async contract needs.

**Architecture:** One Go process with one `whatsmeow.Client` per WhatsApp Account, all stored in a single SQLite file (`$RELAY_DATA_DIR/relay.db`) that also holds the relay's own tables (`accounts`, `sends`, `outbox`, `outbox_dead`). Sending is asynchronous (202 + callback). Every relay→Frappe callback goes through a persistent, ordered outbox. Frappe keeps its existing endpoints with small extensions.

**Tech Stack:** Go 1.27 (`$HOME/.local/go/bin/go`, not on PATH; always `export PATH=$HOME/.local/go/bin:$PATH`), `go.mau.fi/whatsmeow` (pin to `v0.0.0-20261030195514-56e5ad0581ce`), `modernc.org/sqlite v1.61.0` (pure Go, driver name `"sqlite"`, whatsmeow dialect `"sqlite3"`), `github.com/skip2/go-qrcode`, stdlib `net/http` + `log/slog`. Frappe v16 / Python for the app side.

**Spec:** `docs/superpowers/specs/2026-10-31-whatsmeow-relay-design.md`. Read it fully. Section numbers below (§) refer to it.

## Global Constraints

- Go module root: `yourwhatsapp/yourwhatsapp/relay/` (module path `yourwhatsapp/relay`). Node files there (`server.js`, `session_manager.js`, `frappe_client.js`, `package*.json`, `node_modules/`, `.sessions/`) stay untouched until Task 10.
- `CGO_ENABLED=0` must build. No cgo dependencies.
- Env: `RELAY_API_KEY` (required), `FRAPPE_BASE_URL` (default `http://127.0.0.1:8001`), `FRAPPE_SITE_NAME` (optional, sets `Host` header), `RELAY_PORT` (default `8800`), `RELAY_HOST` (default `127.0.0.1`), `RELAY_DATA_DIR` (default `./data`), `SEND_DELAY_MIN` / `SEND_DELAY_MAX` (Go durations, default `4s` / `12s`).
- SQLite DSN: `file:<dir>/relay.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)`; `db.SetMaxOpenConns(1)`.
- API key compared with `crypto/subtle.ConstantTimeCompare`; Python side uses `hmac.compare_digest`.
- Request body limit 25 MB.
- Never log message bodies, media, QR codes or keys. `slog` JSON to stdout, attribute `session_id` on per-account lines.
- Phone normalization: digits only, 8–15 digits, else `invalid_number`.
- Error codes (exact strings): `bad_request`, `invalid_number`, `session_unavailable`, `temp_banned`, `send_failed`, `duplicate`.
- Per-account queue capacity 50.
- Outbox backoff `min(1s * 2^attempts, 5m)`; 401/403 retry every 60 s forever; 404 retried up to 24 h after `created_at`, then dead; other 4xx dead immediately.
- Frappe code: tabs, existing app conventions; tests under `yourwhatsapp/yourwhatsapp/tests/` run with `bench --site whatsapp.localhost run-tests --app yourwhatsapp --module <module>` from `/workspace/development/onebook-bench` (enable `allow_tests` in site config if needed).
- Copy rule: nothing anywhere may say "ban-proof", "Meta-approved", or similar.
- Commit after each task, on branch `feat/whatsmeow-relay`. Messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **Outbox ordering under failure:** a `Sent` callback for a message must never be delivered after a `Delivered`/`Read` receipt for it, even when the head row is being retried. Single worker, strict `id` order, head-of-line blocking is intended (Task 3 test `TestWorkerHeadOfLineBlocks`).
2. **Duplicate send across failover:** the same `local_message_id` sent to session B while session A still has it `queued` must return `duplicate` (Task 2 `TestReserveDuplicateAcrossSessions`, Task 6 `TestSendDuplicateReturns200`).
3. **Relay restart with queued sends:** queued rows become `failed`/`send_failed` and a `message_status` Failed callback is enqueued per row (Task 2 `TestFailQueued`, Task 5 `TestRestoreFailsQueued`).
4. **Paused account in Frappe receives relay `Active`:** must stay `Paused` (Task 7 `test_update_qr_active_ignored_when_paused`).
5. **Inbound from an unresolved LID, then a CRM reply to it:** reply must go out with `to_is_lid=true`, not be number-checked (Task 4 `TestInboundUnresolvedLID`, Task 8 `test_reply_to_lid_sender_sets_flag`).

---

### Task 1: Module scaffold, config, store

**Files:**
- Create: `relay/go.mod`, `relay/Makefile`, `relay/.gitignore` (`bin/`, `data/`), `relay/internal/config/config.go`, `relay/internal/config/config_test.go`, `relay/internal/store/store.go`, `relay/internal/store/accounts.go`, `relay/internal/store/store_test.go`
(all paths in Go tasks are under `yourwhatsapp/yourwhatsapp/relay/`)

**Interfaces:**
- Produces:
  - `config.Config{APIKey, FrappeBaseURL, FrappeSiteName, Host string; Port int; DataDir string; SendDelayMin, SendDelayMax time.Duration}`; `config.Load() (config.Config, error)`, which errors when `RELAY_API_KEY` is empty or `SendDelayMin > SendDelayMax`.
  - `store.Open(ctx context.Context, dataDir string) (*store.Store, error)`: creates the dir, opens SQLite, runs whatsmeow `sqlstore.NewWithDB(db, "sqlite3", nil).Upgrade(ctx)`, then creates our tables (§6) idempotently.
  - `type Store struct { DB *sql.DB; Container *sqlstore.Container }`, `Close() error`.
  - `type Account struct { SessionID, JID, Proxy string; Paused bool }`.
  - `(*Store) UpsertAccount(ctx, sessionID string) error`, `SetAccountJID(ctx, sessionID, jid string) error` (empty string → NULL), `SetAccountPaused(ctx, sessionID string, paused bool) error`, `SetAccountProxy(ctx, sessionID, proxy string) error`, `GetAccount(ctx, sessionID string) (Account, bool, error)`, `ListRestorable(ctx) ([]Account, error)` (`jid` not null and not paused).

- [ ] **Step 1:** `go mod init yourwhatsapp/relay`; `go get` the three pinned deps; `Makefile` targets `build` (`CGO_ENABLED=0 go build -o bin/relay ./cmd/relay`), `test` (`go test ./...`), `run`.
- [ ] **Step 2: Failing tests.** `TestLoadDefaults` (only `RELAY_API_KEY` set → Port 8800, Host `127.0.0.1`, DataDir `./data`, delays 4s/12s, FrappeBaseURL `http://127.0.0.1:8001`); `TestLoadRequiresKey`; `TestLoadRejectsInvertedDelays`; `TestOpenCreatesTables` (tables `accounts`, `sends`, `outbox`, `outbox_dead`, `whatsmeow_device` exist; second `Open` on same dir succeeds); `TestAccountsRoundTrip` (upsert twice is fine, SetAccountJID then ListRestorable returns it, paused excluded, empty JID → excluded).
- [ ] **Step 3:** Run `go test ./internal/...`. Expect FAIL.
- [ ] **Step 4:** Implement.
- [ ] **Step 5:** `go test ./internal/... && CGO_ENABLED=0 go vet ./...`. Expect PASS.
- [ ] **Step 6:** Commit `feat(relay): Go module scaffold, config and SQLite store`.

### Task 2: Phone normalization and sends (idempotency) store

**Files:**
- Create: `relay/internal/phone/phone.go`, `phone_test.go`, `relay/internal/store/sends.go`, `sends_test.go`

**Interfaces:**
- Consumes: `store.Store` (Task 1).
- Produces:
  - `phone.Normalize(raw string) (string, error)`; the error is `phone.ErrInvalid`.
  - `type SendRecord struct { LocalID, SessionID, State, MessageID, ErrorCode string }`; state constants `store.SendQueued = "queued"`, `SendSent = "sent"`, `SendFailed = "failed"`.
  - `(*Store) ReserveSend(ctx, localID, sessionID string) (existing *SendRecord, err error)`: returns non-nil `existing` (and writes nothing) when a row exists with state queued or sent, on any session. Otherwise it inserts or overwrites the row as `queued` for `sessionID` and returns nil.
  - `MarkSent(ctx, localID, messageID string) error`, `MarkFailed(ctx, localID, code string) error`, `FailQueued(ctx) ([]SendRecord, error)` (sets every queued row to failed/`send_failed` and returns them).

- [ ] **Step 1: Failing tests.**
  - `TestNormalize`: table: `"+91 98765 43210"`→`"919876543210"`; `"(415) 555-0100"`→`"4155550100"`; `"1234567"`→ErrInvalid; 16 digits→ErrInvalid; `""`→ErrInvalid.
  - `TestReserveFresh`.
  - `TestReserveDuplicateAcrossSessions`: reserve on A, then reserve on B → existing.SessionID == A.
  - `TestReserveAfterFailedAllowed`: failed on A, then reserve on B → nil, row session B.
  - `TestReserveSentIsDuplicate`: existing.MessageID set.
  - `TestFailQueued`: two queued and one sent → returns 2, both now failed/`send_failed`.
- [ ] **Step 2:** Run. FAIL. **Step 3:** Implement. **Step 4:** Run. PASS.
- [ ] **Step 5:** Commit `feat(relay): phone normalization and idempotent send records`.

### Task 3: Outbox store and worker

**Files:**
- Create: `relay/internal/store/outbox.go`, `outbox_test.go`, `relay/internal/outbox/worker.go`, `relay/internal/outbox/poster.go`, `relay/internal/outbox/worker_test.go`

**Interfaces:**
- Consumes: `store.Store`.
- Produces:
  - `type OutboxRow struct { ID int64; Method string; Payload []byte; Attempts int; NextAttemptAt, CreatedAt time.Time; LastError string }`.
  - `(*Store) EnqueueCallback(ctx, method string, payload any) error` (JSON-marshals the payload), `HeadCallback(ctx) (*OutboxRow, error)` (lowest id, or nil), `DeleteCallback(ctx, id int64) error`, `RetryCallback(ctx, id int64, next time.Time, errMsg string) error` (increments attempts), `KillCallback(ctx, id int64, errMsg string) error` (moves to `outbox_dead`), `ListDead(ctx, limit int) ([]OutboxRow, error)`.
  - Method constants: `outbox.MethodInbound = "inbound_message"`, `MethodStatus = "message_status"`, `MethodQR = "update_qr"`, `MethodAccountEvent = "account_event"`.
  - `type Poster interface { Post(ctx context.Context, method string, payload []byte) (status int, err error) }`.
  - `outbox.NewFrappePoster(baseURL, siteName, apiKey string) Poster`: POSTs JSON to `<base>/api/method/yourwhatsapp.yourwhatsapp.api.<method>` with headers `X-Api-Key`, `Content-Type: application/json`, and `Host: siteName` when set. Timeout 30 s.
  - `outbox.Decide(status int, err error, row OutboxRow, now time.Time) (action Action, next time.Time)` with `Action` ∈ `ActionDelete`, `ActionRetry`, `ActionDead`, following the Global Constraints rules. Network error or 5xx or 429 → retry with backoff; 401/403 → retry at now+60s; 404 → retry with backoff unless `now - CreatedAt > 24h`, then dead.
  - `outbox.NewWorker(st *store.Store, p Poster, log *slog.Logger) *Worker`, `(*Worker) Run(ctx context.Context)` (loop: head row; if `NextAttemptAt` is in the future, sleep until then or until `Notify`; post; apply the decision), `(*Worker) Notify()` (non-blocking wake).
  - `outbox.Backoff(attempts int) time.Duration`.

- [ ] **Step 1: Failing tests.**
  - `TestBackoff`: 0→1s, 3→8s, 20→5m.
  - `TestDecide`: table covering 200→Delete; 500→Retry; network err→Retry; 429→Retry; 401→Retry at +60s; 403→Retry at +60s; 404 young→Retry; 404 older than 24h→Dead; 400→Dead; 422→Dead.
  - `TestWorkerDeliversInOrder`: fake Poster records methods; enqueue 3 → delivered in id order, outbox empty.
  - `TestWorkerHeadOfLineBlocks`: Poster returns 500 for row 1 the first time → row 2 not posted before row 1 succeeds.
  - `TestWorkerDeadLetters`: 400 → row in `outbox_dead`, next row delivered.
  - `TestFrappePosterHeaders` (httptest server asserts path, `X-Api-Key`, Host).
- [ ] **Step 2:** Run. FAIL. **Step 3:** Implement. **Step 4:** Run with `-race`. PASS.
- [ ] **Step 5:** Commit `feat(relay): persistent ordered callback outbox`.

### Task 4: Event conversion (pure functions)

**Files:**
- Create: `relay/internal/convert/inbound.go`, `receipt.go`, `outgoing.go`, `convert_test.go`

**Interfaces:**
- Produces:
  - `convert.IsDirectChat(jid types.JID) bool`: server is `types.DefaultUserServer` or `types.HiddenUserServer`.
  - `type InboundPayload struct` with JSON tags exactly: `session_id, from_number, content_type, message, message_id, is_reply, reply_to_message_id, from_lid, media_base64, media_mimetype, media_filename` (media fields `omitempty`).
  - `convert.Inbound(sessionID string, evt *events.Message, resolvePN func(types.JID) (types.JID, bool)) (p InboundPayload, media whatsmeow.DownloadableMessage, ok bool)`: `ok=false` for IsFromMe, non-direct chats, and protocol/reaction/edit or empty messages. Content types per §8.4. Placeholders for unsupported types: `"[sticker]"`, `"[location]"`, `"[contact]"`, `"[poll]"`, `"[unsupported]"`. `media` is non-nil for image/video/audio/document. `media_filename` = document file name, else `<message_id><ext>` with ext from `mime.ExtensionsByType` (fallback `.bin`); also returned via `p`.
  - `convert.ReceiptStatus(t types.ReceiptType) (status string, ok bool)`: Delivered (`""`)→`"Delivered"`; Read/ReadSelf/Played→`"Read"`; others → ok false.
  - `convert.ContentToMediaType(contentType string) whatsmeow.MediaType` and `convert.BuildMediaMessage(contentType, caption, mimetype, filename string, up whatsmeow.UploadResponse) *waE2E.Message` (unknown content type with attachment → document).
  - `convert.TextMessage(text string) *waE2E.Message`.

- [ ] **Step 1: Failing tests** (build `events.Message` values directly):
  - `TestInboundText`.
  - `TestInboundExtendedText`.
  - `TestInboundDropsFromMe`.
  - `TestInboundDropsGroup` (`g.us`), `TestInboundDropsStatusBroadcast`, `TestInboundDropsNewsletter`.
  - `TestInboundResolvedLID`: resolver returns PN → from_number digits, from_lid false.
  - `TestInboundUnresolvedLID`: from_number = LID user part, from_lid true.
  - `TestInboundImageWithCaption`: content_type image, message = caption, media non-nil, filename `<id>.jpg` for `image/jpeg`.
  - `TestInboundDocumentKeepsFileName`.
  - `TestInboundVoiceNoteIsAudio`.
  - `TestInboundQuotedReply`: ContextInfo.StanzaID → is_reply true and reply_to_message_id.
  - `TestInboundStickerPlaceholder`.
  - `TestInboundDropsReaction`.
  - `TestReceiptStatus` (table).
  - `TestBuildMediaMessageDocumentFallback`.
- [ ] **Step 2:** Run. FAIL. **Step 3:** Implement. **Step 4:** Run. PASS.
- [ ] **Step 5:** Commit `feat(relay): whatsmeow event conversion`.

### Task 5: Session and Manager (lifecycle, pairing, events, send queue)

**Files:**
- Create: `relay/internal/session/client.go` (interface + whatsmeow adapter), `session.go`, `manager.go`, `queue.go`, `session_test.go`, `manager_test.go`, `queue_test.go`

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces:
  - `type WAClient interface`: the subset of `*whatsmeow.Client` used (`Connect() error; Disconnect(); IsConnected() bool; IsLoggedIn() bool; Logout(ctx) error; GetQRChannel(ctx) (<-chan whatsmeow.QRChannelItem, error); PairPhone(ctx, phone string, push bool, ct whatsmeow.PairClientType, name string) (string, error); IsOnWhatsApp(ctx, []string) ([]types.IsOnWhatsAppResponse, error); Upload(ctx, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error); SendMessage(ctx, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error); SendChatPresence(ctx, types.JID, types.ChatPresence, types.ChatPresenceMedia) error; Download(ctx, whatsmeow.DownloadableMessage) ([]byte, error); SetProxyAddress(string, ...whatsmeow.SetProxyOptions) error; AddEventHandler(whatsmeow.EventHandler) uint32; OwnJID() types.JID`) and a `ClientFactory func(dev *store.Device) WAClient`. Tests use a fake.
  - Status constants (§7): `StatusNotStarted = "not_started"`, `StatusQRPending = "qr_pending"`, `StatusPairing = "pairing"`, `StatusConnecting = "connecting"`, `StatusConnected = "connected"`, `StatusDisconnected = "disconnected"`, `StatusLoggedOut = "logged_out"`, `StatusPaused = "paused"`, `StatusTempBanned = "temp_banned"`.
  - `type SendRequest struct { SessionID, To, ContentType, Message, LocalID, AttachBase64, AttachMimetype, AttachFilename string; ToIsLID bool }`.
  - `type SendError struct { Code string; HTTP int; Detail string; Existing *store.SendRecord }`, implementing `error`.
  - `session.NewManager(st *store.Store, enqueue func(ctx context.Context, method string, payload any) error, factory ClientFactory, cfg config.Config, log *slog.Logger) *Manager`.
  - `(*Manager) RestoreAll(ctx) error`: first `FailQueued` + a `message_status` Failed callback per row (`{local_message_id, status:"Failed", error_code:"send_failed"}`), then connect every `ListRestorable` account.
  - `(*Manager) Start(ctx, sessionID string, proxy *string) (string, error)`: idempotent. It works after logout or disconnect: drops the stale Session and builds a new one.
  - `(*Manager) PairCode(ctx, sessionID, rawPhone string) (string, error)`.
  - `(*Manager) Status(sessionID string) (status, jid, phone string)`.
  - `(*Manager) Pause(ctx, id) error`, `Resume(ctx, id string, proxy *string) error`, `Logout(ctx, id) error`.
  - `(*Manager) Send(ctx, req SendRequest) *SendError`: nil means accepted (202). Implements the §8.3 synchronous part.
  - `(*Manager) Shutdown(ctx) error`: stop queues, wait up to 10 s per in-flight send, `Disconnect()` all.
  - The queue (`queue.go`): `newSendQueue(capacity int, delayMin, delayMax time.Duration, sleep func(time.Duration), rnd func() float64)`, injectable for tests. The worker performs §8.3 steps 4–7, including presence `composing`, sleeping `1s + min(len(text)/50, 2)s`, then `paused`.
  - Event handling per §8.2 and §8.4–8.5: callbacks via `enqueue`. QR PNG via `qrcode.Encode(code, qrcode.Medium, 256)` → base64 → `update_qr{session_id, qr_base64}`. Status pushes → `update_qr{session_id, status}` with `Active` / `Disconnected`. Temp ban → `account_event{session_id, event:"temp_banned", until: RFC3339}`, then reconnect after `Expire` via `time.AfterFunc`. Device props: `store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_CHROME.Enum()` and `store.SetOSInfo("Linux", [3]uint32{0,1,0})` once at manager init. PairPhone uses `whatsmeow.PairClientChrome, "Chrome (Linux)"`.

- [ ] **Step 1: Failing tests** (fake WAClient, in-memory store via `t.TempDir()`, zero delays):
  - `TestStartIdempotent`.
  - `TestStartAfterLogoutRebuilds`.
  - `TestQRCodeEnqueuesUpdateQR`.
  - `TestPairSuccessStoresJIDAndActive`.
  - `TestLoggedOutClearsJIDAndDisconnected`.
  - `TestStreamReplacedNoReconnect`.
  - `TestTemporaryBanEnqueuesAccountEvent`.
  - `TestRestoreFailsQueued`.
  - `TestRestoreConnectsRestorable`.
  - `TestSendSessionUnavailable`.
  - `TestSendInvalidNumber`: IsOnWhatsApp returns not-registered → `invalid_number`, 422.
  - `TestSendToLIDSkipsCheck`.
  - `TestSendDuplicate`: `duplicate`, 200, Existing set.
  - `TestSendQueueFull`: 409.
  - `TestQueueSuccessMarksSentAndCallback`: `sends` sent and `message_status{local_message_id, message_id, status:"Sent"}` enqueued.
  - `TestQueueErrorMarksFailedAndCallback`.
  - `TestQueueDelayWithinBounds` (rnd 0 → min, rnd 1 → max).
  - `TestInboundEnqueuesWithMedia`: fake Download → base64 present.
  - `TestInboundDownloadFailureStillDelivers`.
  - `TestReceiptEnqueuesStatus`.
  - `TestShutdownDisconnectsAll`.
- [ ] **Step 2:** Run with `-race`. FAIL. **Step 3:** Implement. **Step 4:** Run with `-race`. PASS.
- [ ] **Step 5:** Commit `feat(relay): whatsmeow session manager with pairing, events and send queue`.

### Task 6: HTTP API, main, graceful shutdown

**Files:**
- Create: `relay/internal/api/api.go`, `api_test.go`, `relay/cmd/relay/main.go`

**Interfaces:**
- Consumes: Tasks 1–5.
- Produces:
  - `api.NewHandler(m Manager, st *store.Store, apiKey string, log *slog.Logger) http.Handler`, where `api.Manager` is an interface matching `*session.Manager`'s public methods (for fakes). Routes and contract per §7 exactly: `/health` open; everything else requires the key → 401 `{"error":"invalid api key"}`; `/message/send` → 202 `{"state":"queued"}`, 200 `{"error":"duplicate","state","message_id"}`, or an error status with `{"error","detail"}`. `GET /outbox/dead` → list. Body limit 25 MB via `http.MaxBytesReader`.
  - `main`: `config.Load` → `store.Open` → `outbox.NewWorker(...).Run` goroutine → `session.NewManager` (enqueue = `st.EnqueueCallback` + `worker.Notify`) → `RestoreAll` → `http.Server{Addr: host:port}`. On SIGINT/SIGTERM: `srv.Shutdown(10s)`, `manager.Shutdown(15s)`, cancel worker, `st.Close()`.

- [ ] **Step 1: Failing tests** (httptest, fake Manager):
  - `TestHealthOpen`.
  - `TestAuthRequired`: missing → 401, wrong → 401.
  - `TestStartPassesProxy`.
  - `TestPairCode`.
  - `TestSendAccepted202`.
  - `TestSendDuplicateReturns200`.
  - `TestSendErrorMapping`: each code → its HTTP status.
  - `TestSendBadJSON400`.
  - `TestSendMissingLocalID400`.
  - `TestBodyTooLarge`: 413.
  - `TestDeadList`.
- [ ] **Step 2:** Run. FAIL. **Step 3:** Implement. **Step 4:** `make test && make build`. PASS, and `bin/relay` exists.
- [ ] **Step 5: Smoke:** `RELAY_API_KEY=x RELAY_PORT=18800 RELAY_DATA_DIR=$(mktemp -d) ./bin/relay &`; `curl -s localhost:18800/health` → `{"ok":true}`; `curl -s -XPOST -H 'x-api-key: x' localhost:18800/session/t1/start` → `{"status":"qr_pending"}` (or `connecting`); `kill -TERM` → exits within 25 s and logs shutdown.
- [ ] **Step 6:** Commit `feat(relay): HTTP API and main with graceful shutdown`.

### Task 7: Frappe: fields, api.py callbacks

**Files:**
- Modify: `yourwhatsapp/yourwhatsapp/doctype/whatsapp_message/whatsapp_message.json` (add `is_lid_sender` Check, read-only; `attempted_accounts` Small Text, hidden), `doctype/whatsapp_account/whatsapp_account.json` (add `proxy` Data; `rate_limited_until` Datetime, read-only; `pairing_method` Select `QR Code\nPhone Number`, default `QR Code`), `yourwhatsapp/yourwhatsapp/api.py`
- Create: `yourwhatsapp/yourwhatsapp/tests/__init__.py`, `tests/test_api.py`

**Interfaces:**
- Produces:
  - `api.message_status(status, message_id=None, error_reason=None, local_message_id=None, error_code=None)`.
  - `api.inbound_message(..., from_lid: bool = False)`.
  - `api.account_event(session_id, event, until=None)`.
  - `api.STATUS_RANK = {"Queued": 0, "Sent": 1, "Delivered": 2, "Read": 3}`.
  - `api.handle_async_failure(message_name: str, error_code: str)` is **declared here as a call into** `relay_client.handle_async_failure`, which Task 8 implements. In this task, `message_status` calls `frappe.enqueue("yourwhatsapp.yourwhatsapp.relay_client.handle_async_failure", message_name=..., error_code=...)`.
  - `_verify_relay_key` uses `hmac.compare_digest`.
  - `update_qr(status=...)`: ignores `Active`/`Disconnected` when the account is `Paused`. Ignores `Active` when the account is `RateLimited` and (`rate_limited_until` is empty or in the future).

- [ ] **Step 1: Failing tests** (`FrappeTestCase`, set the relay key in Settings, set the `X-Api-Key` header via `frappe.local.request` stub or by patching `frappe.get_request_header`):
  - `test_message_status_by_local_id_sets_message_id`.
  - `test_message_status_monotonic` (Read then Delivered → stays Read).
  - `test_failed_ignored_after_sent`.
  - `test_failed_enqueues_failover` (patch `frappe.enqueue`).
  - `test_invalid_number_failed_no_failover`.
  - `test_unknown_id_noop`.
  - `test_inbound_dedupe`.
  - `test_inbound_from_lid_sets_flag`.
  - `test_account_event_temp_banned`: status RateLimited, `rate_limited_until` set.
  - `test_update_qr_active_ignored_when_paused`.
  - `test_update_qr_active_ignored_while_banned`.
  - `test_bad_key_rejected`.
- [ ] **Step 2:** `bench --site whatsapp.localhost migrate`, then run tests. FAIL.
- [ ] **Step 3:** Implement. **Step 4:** Run. PASS.
- [ ] **Step 5:** Commit `feat: async-aware relay callbacks, LID flag, temp-ban event`.

### Task 8: Frappe: relay_client async contract, failover, scheduler

**Files:**
- Modify: `yourwhatsapp/yourwhatsapp/relay_client.py`, `yourwhatsapp/yourwhatsapp/tasks.py`, `yourwhatsapp/hooks.py` (add `"cron": {"*/5 * * * *": ["yourwhatsapp.yourwhatsapp.tasks.recover_rate_limited_accounts"]}`)
- Create: `yourwhatsapp/yourwhatsapp/tests/test_relay_client.py`

**Interfaces:**
- Consumes: Task 7 fields and `message_status`.
- Produces:
  - `relay_client.TIMEOUT = 30`.
  - `relay_client.send_message(message_name)` (same entry point).
  - `relay_client.handle_async_failure(message_name: str, error_code: str)`.
  - `relay_client.increment_daily_count(account: str)` (atomic SQL `UPDATE`); called by `api.message_status` on `Sent` (add that call in this task).
  - `tasks.recover_rate_limited_accounts()`.
  - Behaviour per §9:
    - Payload adds `local_message_id=doc.name` and `to_is_lid`.
    - `attempted_accounts` JSON list maintained on the message.
    - Failover picks via `routing.pick_account(exclude=attempted)`.
    - Remove the old `daily_message_count` read-modify-write.

- [ ] **Step 1: Failing tests** (patch `requests.post`):
  - `test_202_leaves_queued`.
  - `test_duplicate_noop`.
  - `test_422_failed_no_failover`.
  - `test_409_fails_over_to_next_account` (two Active accounts).
  - `test_429_sets_rate_limited_and_fails_over`.
  - `test_timeout_retries_same_account_once`.
  - `test_all_accounts_exhausted_failed`.
  - `test_async_failure_fails_over_excluding_attempted`.
  - `test_reply_to_lid_sender_sets_flag`.
  - `test_sent_callback_increments_count_atomically`.
  - `test_recover_rate_limited_after_until`.
  - `test_manual_rate_limited_not_recovered` (no until).
- [ ] **Step 2:** Run. FAIL. **Step 3:** Implement. **Step 4:** Run both test modules. PASS.
- [ ] **Step 5:** Commit `feat: async relay send contract with safe failover and rate-limit recovery`.

### Task 9: Frappe: account form controls

**Files:**
- Modify: `doctype/whatsapp_account/whatsapp_account.py`, `whatsapp_account.js`
- Create: `tests/test_whatsapp_account.py`

**Interfaces:**
- Consumes: relay routes (§7).
- Produces whitelisted methods on `WhatsAppAccount`, all behind the existing role check (factor it into `_check_admin()`):
  - `connect()`: sends `{"proxy": self.proxy or None}`.
  - `pair_with_code()`: requires `phone_number` and returns `{"code"}`.
  - `pause()`: sets status `Paused` after the relay call.
  - `resume()`: sets status `Disconnected` until the relay reports `Active`.
  - `logout()`: sets `Disconnected` and clears `connected_at`.
- JS:
  - The Connect button follows `pairing_method`. Phone Number → dialog showing the code in large monospace, plus the instruction text "On the phone: WhatsApp → Linked Devices → Link a device → Link with phone number instead, then enter this code."
  - Buttons Pause (when not Paused), Resume (when Paused), Logout (with `frappe.confirm`).
  - Keep the existing QR polling.

- [ ] **Step 1: Failing tests** (patch `requests.post`): `test_connect_sends_proxy`, `test_pair_with_code_requires_phone`, `test_pause_sets_paused`, `test_logout_sets_disconnected`, `test_non_admin_forbidden`.
- [ ] **Step 2:** Run. FAIL. **Step 3:** Implement. **Step 4:** Run. PASS. Then `bench build --app yourwhatsapp`, which must succeed.
- [ ] **Step 5:** Commit `feat: pairing-code, pause/resume and logout controls on WhatsApp Account`.

### Task 10: Packaging, deploy docs, bench wiring, docs

**Files:**
- Create: `relay/Dockerfile`, `relay/compose.whatsapp.yaml`, `DEPLOYMENT.md`
- Modify: `relay/README.md` (rewrite for Go), `/workspace/development/onebook-bench/Procfile` (`relay:` → `apps/yourwhatsapp/yourwhatsapp/yourwhatsapp/relay/bin/relay`; note: not in git, a bench-local file), `CLAUDE.md` (running-locally section: `make build`, Go path), `README.md` (relay section)

**Interfaces:**
- Dockerfile per §12: `golang:1.27-alpine` build stage with `CGO_ENABLED=0`; runtime `gcr.io/distroless/static:nonroot`; `ENV RELAY_HOST=0.0.0.0 RELAY_DATA_DIR=/data`; `VOLUME /data`; `EXPOSE 8800`. Distroless has no shell for `HEALTHCHECK`, so add a `relay healthcheck` subcommand in `main.go` that GETs `/health` and exits 0/1, and use `HEALTHCHECK CMD ["/relay","healthcheck"]`.
- Compose file: the service block from the conversation (no published ports, `stop_grace_period: 30s`, named volume, env `FRAPPE_BASE_URL`, `FRAPPE_SITE_NAME`, `RELAY_API_KEY`).
- `DEPLOYMENT.md`:
  1. How frappe_docker builds the Frappe image with `yourwhatsapp` (`apps.json`).
  2. Building and pushing the relay image.
  3. Adding the compose override.
  4. WhatsApp Settings (relay URL `http://whatsapp-relay:8800`, key).
  5. Linking numbers (QR / code).
  6. Backing up the `whatsapp-relay-data` volume.
  7. Warm-up advice.
  8. The explicit statement that this is risk reduction, not ban-proof.
  9. A note that managed hosts that can't add containers need a separate relay host.

- [ ] **Step 1:** Add the `healthcheck` subcommand plus test `TestHealthcheckCommand` (httptest). Run. PASS.
- [ ] **Step 2:** Write the Dockerfile, compose file and docs. Docker isn't available here: validate the compose file with `python3 -c "import yaml,sys; yaml.safe_load(open(sys.argv[1]))" relay/compose.whatsapp.yaml`.
- [ ] **Step 3:** `make build` and `make test`. PASS.
- [ ] **Step 4:** Commit `build(relay): Dockerfile, compose override and deployment guide`.

### Task 11 (controller, with the user): cutover and live verification

Not for a subagent; needs the user's phone.

- [ ] Stop `bench start` cleanly (Ctrl+C, not Ctrl+Z). Kill any `chromium-headless-shell` under `relay/.sessions/`.
- [ ] `make build`; `bench start`; confirm the Go relay is on :8800 (`/health`).
- [ ] With the user:
  1. Relink each number (QR on one, pairing code on another).
  2. Send and receive text.
  3. Send and receive media (image, PDF, voice note).
  4. Check receipts reach Read.
  5. Restart the relay → sessions come back without a rescan.
  6. Stop the web process, send an inbound, start the web process → message arrives.
  7. Pause one account → failover to the other.
  8. Logout from the phone → account shows Disconnected.
- [ ] Then delete the Node relay files and update the `CLAUDE.md` gotchas (remove the browser-specific ones, add whatsmeow ones learned). Mark `ROADMAP.md` §0 done. Commit.
