# Phase 1.6 Blockers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a System Manager claim and manage the relay entirely from the Desk UI. Fill template `{{n}}` from the reference document. Collapse the default-number settings to one auto-filled field. Remove the Node relay.

**Architecture:**
- A new Go package `internal/claim` owns the relay's identity: mode, key, callback URL, site name, and key-rotation grace.
  - `api` uses it for auth and gating.
  - The outbox poster reads the callback target and key from it on every post.
  - `main` starts sessions and the outbox only once claimed.
- Frappe adds settings buttons with a pending-key protocol.
- A new `WhatsApp Template Parameter` child table, with resolution in WhatsApp Message `before_insert`.
- A shared `routing.set_account_active` helper.

**Tech Stack:** Go 1.27 (`$HOME/.local/go/bin`), whatsmeow pinned, modernc sqlite; Frappe v16 / Python.

**Spec:** `docs/superpowers/specs/2026-10-31-phase-1-6-blockers-design.md` (authority; read fully). Earlier spec: `docs/superpowers/specs/2026-10-31-whatsmeow-relay-design.md`.

## Global Constraints

- **Paths:**
  - Go module: `yourwhatsapp/yourwhatsapp/yourwhatsapp/relay/`.
  - Frappe package: `yourwhatsapp/yourwhatsapp/yourwhatsapp/`.
  - Tests: `yourwhatsapp/yourwhatsapp/yourwhatsapp/tests/`.
- **Live system:** bench and the Go relay are running against real phones. Never:
  - restart bench or the relay;
  - run any relay binary on :8800 or against `relay/data/`;
  - send real WhatsApp messages;
  - run patches or tests that touch real site files.
  Tests mock HTTP.
- **Commits:** conventional messages with **no Claude/AI authorship trailer**. No pushing.
- **Keys:** never log the key, put it in error messages, or return it from `/info`.
  - Relay key comparisons use `crypto/subtle.ConstantTimeCompare`; Python uses `hmac.compare_digest`.
  - Generate keys with `secrets.token_urlsafe(32)`; the minimum accepted key length is 32 chars.
- **Relay modes (exact strings):**
  - `env` when `RELAY_API_KEY` is set;
  - `claimed` when a `relay_config` row exists;
  - `unclaimed` otherwise.
  - Protected routes answer 503 `{"error":"unclaimed"}` while unclaimed.
- **Claim error codes (exact):** `already_claimed` (409), `bad_bootstrap_token` (401), `bad_request` (400), `env_mode` (409, for rotate/callback in env mode).
- **Rotation:** the old key stays accepted for incoming requests for 10 minutes; callbacks use the new key immediately.
- **Placeholders:** `{{n}}` with n ≥ 1, regex `\{\{(\d+)\}\}`. A message may never be sent with an unresolved placeholder.
- **No sticky sender:** routing stays rotation (`CLAUDE.md` client requirement).
- **Running tests:**
  - Go: `export PATH=$HOME/.local/go/bin:$PATH; cd <relay> && CGO_ENABLED=1 go test -race ./... && CGO_ENABLED=0 go vet ./... && make build`.
  - Frappe, from `/workspace/development/onebook-bench`: `bench --site whatsapp.localhost migrate && bench --site whatsapp.localhost run-tests --app yourwhatsapp --module <module>`.
  - Run the full app suite once per task.

## Review Focus

1. **Claim races:** two `POST /claim` calls at the same time must yield exactly one success (`TestClaimConcurrentOnlyOneWins`).
2. **Rotation mid-flight:** a callback signed with the new key must be accepted by Frappe while `relay_pending_key` holds it; the old key must be accepted by the relay for 10 minutes and rejected after (`TestRotateGraceWindow`, `test_verify_accepts_pending_key`).
3. **Unclaimed relay on a restored volume:** a `relay_config` row survives a restart, so the relay comes up `claimed` and restores sessions (`TestStartupModeClaimedRestores`).
4. **A template sent from a CRM Deal referencing lead fields:** `lead.first_name` resolves through the Link (`test_resolve_dotted_link_from_deal`).
5. **A template with a placeholder but no reference and no number match** is blocked at insert with a readable error; nothing is enqueued (`test_unresolvable_blocks_insert_no_enqueue`).

---

### Task 1: Relay identity: `claim` package, startup modes, `/claim`, unclaimed gating

**Files:**
- Create: `relay/internal/claim/claim.go`, `relay/internal/claim/claim_test.go`, `relay/internal/store/relayconfig.go` (+ test)
- Modify: `relay/internal/store/store.go` (create the `relay_config` table: `id INTEGER PRIMARY KEY CHECK(id=1), api_key TEXT NOT NULL, callback_url TEXT NOT NULL, site_name TEXT NOT NULL, claimed_at TEXT NOT NULL`), `relay/internal/config/config.go` (`RELAY_API_KEY` becomes optional; add `BootstrapToken` from `RELAY_BOOTSTRAP_TOKEN`), `relay/internal/api/api.go` (+ test), `relay/cmd/relay/main.go`

**Interfaces:**
- Produces:
  - `claim.Mode` with constants `ModeEnv = "env"`, `ModeClaimed = "claimed"`, `ModeUnclaimed = "unclaimed"`.
  - `claim.New(st *store.Store, cfg config.Config, now func() time.Time) (*claim.State, error)` loads the mode.
  - `(*State) Mode() Mode`.
  - `(*State) Authorized(key string) bool` (constant-time; includes the grace key until expiry).
  - `(*State) Target() (callbackURL, siteName, apiKey string)`.
  - `(*State) Claim(ctx, apiKey, callbackURL, siteName, bootstrapToken string) error` returns `claim.ErrAlreadyClaimed`, `ErrBadBootstrapToken` or `ErrBadRequest`.
  - `(*State) OnClaimed(func())` registers a hook run once after a successful claim.
  - `store.(*Store).GetRelayConfig(ctx) (*RelayConfig, error)` and `InsertRelayConfig(ctx, RelayConfig) error`, which fails if a row exists: an atomic `INSERT` on PK `id=1`.
  - `api.NewHandler(m Manager, st *store.Store, cs *claim.State, log *slog.Logger) http.Handler` (replaces the apiKey param).
  - `main`:
    - If claimed or env, run `RestoreAll` and the outbox worker as today.
    - If unclaimed, register `OnClaimed` to start the worker and run `RestoreAll`.
    - The outbox poster gets a `TargetFunc func() (url, site, key string)` from `cs.Target`. Change `outbox.NewFrappePoster(target func() (string, string, string)) Poster` and update its test.
- [ ] **Step 1: Failing tests:**
  - `TestModeEnvWhenKeySet`, `TestModeClaimedFromRow`, `TestModeUnclaimed`
  - `TestClaimHappyPath` (row stored; mode becomes claimed; the hook runs once)
  - `TestClaimAlreadyClaimed`, `TestClaimEnvModeRejected` (409 `already_claimed`)
  - `TestClaimBootstrapRequiredWrongRight`
  - `TestClaimShortKeyOrBadURL400`
  - `TestClaimConcurrentOnlyOneWins` (20 goroutines)
  - `TestUnclaimedProtectedRoutes503` (`/session/x/status`, `/message/send`, `/outbox/dead` → 503 `unclaimed`; `/health` 200)
  - `TestPosterUsesTargetFunc` (URL, Host, `X-Api-Key` from the func, re-read on each post)
  - `TestStartupModeClaimedRestores` (a store with a row → `RestoreAll` is called)
- [ ] **Step 2:** Run. FAIL. **Step 3:** Implement. **Step 4:** Run the Go suite. PASS.
- [ ] **Step 5:** Smoke test on a temp dir and port 18801 with **no** `RELAY_API_KEY`:
  - `/health` → 200.
  - A protected route → 503.
  - `POST /claim` with a 43-char key and `callback_url=http://127.0.0.1:8001` → 200.
  - The protected route with that key → 200.
  - SIGTERM, then kill.
- [ ] **Step 6:** Commit `feat(relay): claimable relay identity and unclaimed gating`.

### Task 2: Relay rotate, callback update, `/info`

**Files:**
- Modify: `relay/internal/claim/claim.go`, `relay/internal/api/api.go` (+ tests), `relay/Makefile` (`-ldflags "-X main.version=$(git describe --always --dirty)"`), `relay/cmd/relay/main.go` (`var version = "dev"`, passed to the handler)

**Interfaces:**
- Consumes: Task 1 `claim.State`.
- Produces:
  - `(*State) Rotate(ctx, newKey string) error`: `ErrEnvMode`, `ErrBadRequest`; persists the key; keeps the old key valid until `now()+10m`.
  - `(*State) SetCallback(ctx, url, site string) error` (`ErrEnvMode`).
  - Routes:
    - `POST /claim/rotate` `{api_key}`
    - `PUT /claim/callback` `{callback_url, site_name}`
    - `GET /info` returns `{version, claimed, mode}` for everyone. With a valid key it also returns `{sessions:{connected,total}, outbox:{pending,dead}, callback_url, site_name}`.
  - `api.Manager` gains `Counts() (connected, total int)`, implemented in `session.Manager`.
  - `store` gains `OutboxCounts(ctx) (pending, dead int, err error)`.
- [ ] **Step 1: Failing tests:**
  - `TestRotateGraceWindow` (fake clock: old key ok at +9m, rejected at +11m; new key ok; `Target()` returns the new key immediately)
  - `TestRotateEnvMode409`
  - `TestSetCallbackPersistsAndTargets`
  - `TestInfoUnauthenticatedMinimal` (no counts, never a key)
  - `TestInfoAuthenticatedCounts`
  - `TestInfoNeverContainsKey` (assert the key substring is absent in every mode)
- [ ] **Step 2:** Run. FAIL. **Step 3:** Implement. **Step 4:** PASS, `make build` (the binary prints its version in the startup log line).
- [ ] **Step 5:** Commit `feat(relay): key rotation with grace, callback update and /info`.

### Task 3: Frappe settings: Connect / Rotate / Update Callback / Test, pending key

**Files:**
- Modify:
  - `doctype/whatsapp_settings/whatsapp_settings.json`:
    - add `relay_pending_key` (Password, hidden) and `relay_bootstrap_token` (Password);
    - relabel `relay_reachable_frappe_url` "Callback URL (Frappe as reachable from the relay)" with help text per spec §6.3;
    - `relay_api_key` read-only unless the relay mode is env (handled in JS);
    - add `relay_status_html` (HTML).
  - `whatsapp_settings.py`, `whatsapp_settings.js`, `api.py` (`_verify_relay_key` accepts current or pending).
- Create: `tests/test_whatsapp_settings.py`

**Interfaces:**
- Consumes: relay routes (Tasks 1–2).
- Produces whitelisted methods on `WhatsAppSettings`, System Manager only:
  - `connect_relay() -> dict` (GET `/info` first: env → return `{"mode":"env","message":...}`; else the pending-key protocol around POST `/claim`).
  - `rotate_key()`
  - `update_callback()`
  - `test_connection() -> dict` (the `/info` body plus `{"state": "ok"|"unreachable"|"key_rejected"|"unclaimed"}`).
  - `retry_pending()` (re-sends the claim or rotate for a leftover pending key).
  - The callback URL defaults to `frappe.utils.get_url()` when empty.
- [ ] **Step 1: Failing tests** (patch `requests`):
  - `test_connect_saves_key_only_on_success`
  - `test_connect_pending_survives_failure` (relay 500 → `relay_api_key` unchanged, pending set)
  - `test_connect_env_mode_message`
  - `test_connect_already_claimed_message`
  - `test_connect_sends_bootstrap_and_site`
  - `test_rotate_uses_old_key_and_swaps_on_200`
  - `test_rotate_failure_keeps_old`
  - `test_update_callback`
  - `test_test_connection_states` (unreachable / 401 / unclaimed / ok)
  - `test_verify_accepts_pending_key`
  - `test_non_system_manager_forbidden`
  - `test_key_never_in_error_message`
- [ ] **Step 2:** Migrate and run. FAIL. **Step 3:** Implement (JS renders the panel from `test_connection` on refresh, with buttons per state). **Step 4:** PASS, then `bench build --app yourwhatsapp`.
- [ ] **Step 5:** Commit `feat: claim, rotate and test the relay from WhatsApp Settings`.

### Task 4: Template parameters

**Files:**
- Create: `doctype/whatsapp_template_parameter/` (child table: `placeholder` Int reqd, `field` Data reqd, `default_value` Data), `tests/test_template_parameters.py`
- Modify: `doctype/whatsapp_templates/whatsapp_templates.json` (Table `parameters`) + `.py` (validate), `doctype/whatsapp_message/whatsapp_message.py` (move `_link_crm_reference` into `before_insert` ahead of resolution; resolve), `relay_client.py` (`_render_message` final check → `UnresolvedPlaceholders` → Failed "unresolved template placeholders")

**Interfaces:**
- Produces `yourwhatsapp.yourwhatsapp.templating`:
  - `placeholders(text) -> list[int]`;
  - `resolve(template_name, reference_doctype, reference_name) -> list[str]` raises `frappe.ValidationError` naming the missing placeholders and fields;
  - `render(text, values) -> str`.
- [ ] **Step 1: Failing tests.** Use real doctypes on the site (CRM Lead, CRM Deal, Customer, Sales Order) and create minimal docs:
  - `test_resolve_lead_fields`
  - `test_resolve_dotted_link_from_deal` (`lead.first_name`)
  - `test_resolve_customer_and_sales_order`
  - `test_default_value_used`
  - `test_missing_value_blocks`
  - `test_missing_row_blocks`
  - `test_no_reference_uses_number_match`
  - `test_unresolvable_blocks_insert_no_enqueue` (patch `frappe.enqueue`; assert not called)
  - `test_template_validate_requires_rows_and_valid_fields`
  - `test_render_final_check_fails_message`
  - `test_crm_send_whatsapp_template_end_to_end` (call `crm.api.whatsapp.send_whatsapp_template` with a lead → `template_parameters` filled; `requests` patched)
  - `test_date_formatting`
- [ ] **Step 2:** Run. FAIL. **Step 3:** Implement. **Step 4:** PASS (full app suite).
- [ ] **Step 5:** Commit `feat: fill template placeholders from the reference document`.

### Task 5: One default-number setting

**Files:**
- Modify:
  - `doctype/whatsapp_account/whatsapp_account.json`: remove `is_default_outgoing`, `is_default_incoming`.
  - `whatsapp_account.py`: remove `_enforce_single_default`; use the helper.
  - `routing.py`: add `set_account_active(account, connected_at=True)`.
  - Every place that sets status Active: `api.py` `update_qr` / status callbacks, account `connect` / `resume` / already-paired, `tasks.recover_rate_limited_accounts`.
  - `whatsapp_settings.json`: label/help for the default fields.
- Create: `patches/v1_0/fill_default_whatsapp_accounts.py` (+ `patches.txt`), `tests/test_default_accounts.py`

**Interfaces:**
- Produces: `routing.set_account_active(account: str) -> bool` (respects `relay_may_set_status`; on success auto-fills the Settings defaults if empty or dangling; returns whether it set Active).
- [ ] **Step 1: Failing tests:**
  - `test_first_active_fills_defaults`
  - `test_existing_default_kept`
  - `test_dangling_default_replaced`
  - `test_paused_not_overridden`
  - `test_all_active_paths_use_helper` (each path → defaults filled)
  - `test_patch_fills_when_empty`
  - `test_routing_still_rotates_ignoring_default`
- [ ] **Step 2:** Migrate and run. FAIL. **Step 3:** Implement. **Step 4:** PASS (full suite).
- [ ] **Step 5:** Commit `refactor: single auto-filled default WhatsApp number`.

### Task 6: Docs for claim flow and cutover state

**Files:**
- Modify: `DEPLOYMENT.md`, `relay/README.md`, `relay/compose.whatsapp.yaml`, `CLAUDE.md`, `ROADMAP.md`.
- Changes:
  - **DEPLOYMENT.md:** Settings walkthrough with Connect Relay, bootstrap token, Test / Rotate / Update Callback; recovery = wipe the relay volume and claim again; template parameter setup.
  - **compose:** `RELAY_API_KEY` removed from the required env; `RELAY_BOOTSTRAP_TOKEN` added as recommended; `FRAPPE_*` envs optional (fallback only).
  - **relay README:** new routes and modes.
  - **CLAUDE.md:** the Procfile is already cut over to the Go relay (remove "deliberately not changed yet"); claim modes; the dev relay is env-mode.
  - **ROADMAP:** tick the §A claim, callback/Host and panel items and the §E template and default items.
- [ ] **Step 1:** Write the docs. Validate the compose YAML with the bench python's `yaml.safe_load`.
- [ ] **Step 2:** Commit `docs: claim-based relay setup, template parameters, cutover state`.

### Task 7 (controller + user): live verification and Node removal

- [ ] Ask the user to restart bench (Ctrl+C, then `bench start`). Verify `/health`, then `/info` with the key shows `mode=env` and connected sessions equal to the linked count, with no rescan.
- [ ] On a spare port and temp dir (never :8800 or `relay/data`), run a second relay binary with no key. Claim it from a **scratch copy of the settings flow** (call the relay endpoints directly with curl). Verify `/info`, rotate, and callback update. Stop it.
- [ ] With the user: create one template with `{{1}}` mapped to `first_name` and send it from a lead whose number is one of the user's own linked numbers.
- [ ] Ask the user before deleting `relay/.sessions/`. Then delete the Node relay files (spec §9), clean up the `CLAUDE.md` gotchas and `.dockerignore`, remove `CHROMIUM_PATH` from `.env`, and delete the `*.node-backup` files after the user confirms. Commit `chore: remove Node relay`.
