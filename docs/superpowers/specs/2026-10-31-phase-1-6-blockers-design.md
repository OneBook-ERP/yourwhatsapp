# Design: Phase 1.6 blockers — relay claim flow, template parameters, default-number cleanup, Node relay removal

- **Date:** 2026-10-31
- **Status:** Draft, awaiting approval
- **Roadmap:** Phase 1.6 §A (claim flow, callback URL/Host from claim, settings relay panel), §E (template parameters, default-number duplication), §0 (delete Node relay). Plan 2 (status reconciliation, replies/reactions, RBAC audit, health alert, warm-up doc) comes after.
- **Builds on:** `2026-10-31-whatsmeow-relay-design.md` (Go relay, live on the dev bench).

## 1. Intent

Production runs in Docker with **no shell access for us**. After the first `docker compose up`, a System Manager must be able to finish setup from the Desk UI alone. Today the relay needs `RELAY_API_KEY`, `FRAPPE_BASE_URL` and `FRAPPE_SITE_NAME` set as container env vars, and the same key copied into WhatsApp Settings by hand. The "Frappe URL (reachable from relay)" field in Settings is read by no code.

Separately, CRM sends templates with no parameters, so customers receive literal `{{1}}`. That's a go-live blocker.

**Client requirement (binding):** the CRM agent keeps the full per-customer conversation across numbers, with no sticky sender (see `CLAUDE.md`). Nothing here may tie a conversation to one number.

## 2. Goals

1. **Relay claim flow.** The relay boots **unclaimed**, with no key env required. In WhatsApp Settings the admin enters the **Relay Base URL** and clicks **Connect Relay**. Frappe generates a random key and claims the relay, sending the key, its callback URL and its site name. The relay stores these in `relay.db` and refuses further claims.
2. **Callback target comes from the claim.** The callback base URL and the `Host` header come from the claim, replacing the `FRAPPE_BASE_URL` / `FRAPPE_SITE_NAME` env vars. The env vars stay supported as a fallback and for dev.
3. **Settings relay panel.** Shows connection state, relay version, session counts and outbox depth/dead count, with **Test Connection** and **Rotate Key** buttons.
4. **Template parameters from the reference document.** `{{1}}`, `{{2}}`… are filled from fields of the message's reference document, whatever its doctype (CRM Lead, CRM Deal, Customer, Sales Order, any other), with one level of link traversal. A missing required value blocks the send with a clear error. Nothing is sent with a literal placeholder.
5. **One default-number setting.** Remove the per-account `is_default_outgoing` / `is_default_incoming` checkboxes. Keep WhatsApp Settings `default_outgoing_account`: it exists only because CRM's `is_whatsapp_enabled` needs it, and routing ignores it. It is auto-filled with an Active account whenever it is empty or points at a deleted account.
6. **Remove the Node relay.** Delete the Node relay files and the obsolete browser gotchas in `CLAUDE.md`, now that the Go relay is live. Gated on the controller confirming the Go relay survived a bench restart with sessions restored.

## 3. Non-goals

- Status reconciliation job, replies/reactions, RBAC audit, health alerts, warm-up doc (plan 2).
- Multi-tenant relays (one relay serving several Frappe sites). One relay is claimed by exactly one site.
- Header/footer template parameters (body only).
- Jinja or arbitrary expressions in templates.

## 4. Anti-goals

- A relay that can be claimed or re-claimed by anyone who can reach it without a valid key, or without the bootstrap token when one is configured.
- The API key appearing in logs, error messages, or any Frappe field other than the `relay_api_key` Password field.
- Sending a template with any unresolved `{{n}}` left in the body.
- Breaking the running dev setup: the env-var-configured relay must keep working, as "pre-claimed".

## 5. Constraints

- The Go relay and Frappe app conventions from the previous spec apply: pure-Go SQLite, no cgo, tabs in Python, private files, no Claude/AI trailer in commits.
- The live dev bench has real linked numbers. Tests mock HTTP. Nothing restarts bench or the relay, writes to `relay/data/`, or sends real WhatsApp messages. The final cutover task is done with the user.

## 6. Relay claim flow

### 6.1 Relay state
New table `relay_config` (single row): `api_key_hash` (SHA-256 hex of the key), `callback_url`, `site_name`, `claimed_at`.

- **Startup modes:**
  - If `RELAY_API_KEY` is set in env, the relay is **env-claimed**: the key comes from env, and callback/site come from `FRAPPE_BASE_URL` / `FRAPPE_SITE_NAME`. `/claim` answers 409 `already_claimed`. (Dev and back-compat.)
  - Otherwise, if `relay_config` has a row, the relay is **claimed** and uses the stored values.
  - Otherwise it is **unclaimed**:
    - Every route except `/health`, `/claim` and `GET /info` answers 503 `{"error":"unclaimed"}`.
    - The outbox worker is idle.
    - No sessions are restored.
- **Key storage (decided):** plaintext only, in `relay_config.api_key`, compared in constant time. The outbox poster needs the plaintext anyway, and `relay.db` is already a credential. The `api_key_hash` column is dropped from the schema above.
- **Rotation grace:** after `/claim/rotate` the relay accepts **both** old and new key for incoming requests for 10 minutes; callbacks switch to the new key immediately. `/claim/callback` and `/claim/rotate` answer 409 in env mode.
- **`RELAY_BOOTSTRAP_TOKEN`** (optional env). When set, `/claim` requires `bootstrap_token` to match it (constant-time compare). DEPLOYMENT.md recommends setting it in production.

### 6.2 Routes
| Route | Auth | Body → Response |
|---|---|---|
| `POST /claim` | none while unclaimed (+ bootstrap token if configured) | `{api_key, callback_url, site_name, bootstrap_token?}` → 200 `{"claimed":true}`. Errors: 409 `already_claimed`, 401 `bad_bootstrap_token`, 400 `bad_request` (key shorter than 32 chars, callback URL not http(s)). |
| `POST /claim/rotate` | current key | `{api_key}` → 200; the new key takes effect immediately for both auth and callbacks. 409 when env-claimed (the key comes from env). |
| `PUT /claim/callback` | current key | `{callback_url, site_name}` → 200. Lets the admin fix the callback URL without re-claiming. |
| `GET /info` | optional key | Always: `{"version", "claimed": bool, "mode": "env" / "claimed" / "unclaimed"}`. With a valid key, also `{"sessions": {"connected": n, "total": n}, "outbox": {"pending": n, "dead": n}, "callback_url", "site_name"}`. Never returns a key. |

- **Version:** a build-time `-ldflags "-X main.version=..."` from `git describe`, defaulting to `dev`.
- **Claiming at runtime:** after a successful `/claim`, the relay restores sessions (there are none on a fresh volume), starts the outbox worker, and starts serving. No restart is needed.

### 6.3 Frappe side
- **WhatsApp Settings fields:**
  - `relay_base_url`: stays.
  - `relay_api_key`: stays a Password field, now read-only in the UI and set by the buttons.
  - `relay_reachable_frappe_url`: relabel "Callback URL (Frappe as reachable from the relay)". Default `frappe.utils.get_url()`. Help text: in Docker, the internal URL, e.g. `http://frontend:8080`.
  - `relay_bootstrap_token`: new optional Password field.
  - New read-only `relay_status` HTML panel (rendered client-side).
- **Never lose the key (decided):** before calling the relay, Frappe stores the new key in a new Password field `relay_pending_key` and commits. On 200 it moves pending → `relay_api_key` and clears pending. Until then `_verify_relay_key` accepts either the current or the pending key, so callbacks signed with the new key aren't rejected mid-rotation. A leftover pending key is shown in the panel with a **Retry** of the same claim/rotate.
- **Env mode (decided):** Connect Relay first calls `GET /info`. If `mode` is `env`, it explains that this relay's key comes from `RELAY_API_KEY` and must be typed into the field by hand (the field stays editable for that case only).
- **Buttons (System Manager only):**
  - **Connect Relay:** generate the key with `secrets.token_urlsafe(32)`, then POST `/claim` with key, callback URL, `frappe.local.site` and the bootstrap token. On 200, save the key. On 409 `already_claimed`, show "This relay is already claimed. If it's yours, use Rotate Key with the current key, or reset the relay's volume."
  - **Rotate Key:** generate a new key, POST `/claim/rotate` authenticated with the old one, and save the new key only after a 200.
  - **Update Callback URL:** PUT `/claim/callback`.
  - **Test Connection:** GET `/info` with the key and render the panel. Shows "unreachable" on connection error, "key rejected" on 401, "unclaimed" if the relay isn't claimed.
- The existing manual path (typing a key that matches `RELAY_API_KEY`) keeps working for env-claimed relays.

## 7. Template parameters
- **New child doctype `WhatsApp Template Parameter`** (table field `parameters` on WhatsApp Templates), with fields:
  - `placeholder` (Int, 1-based, required);
  - `field` (Data, required): a fieldname on the reference doctype, or `link_field.target_field` for one level of Link traversal;
  - `default_value` (Data, optional).
- **Reference fallback (decided):** if the message has no reference, the existing number-based CRM Lead/Deal lookup (`_link_crm_reference`) runs first, moved into `before_insert` before resolution. One level of link traversal covers e.g. Deal → `lead.first_name`. Resolution reads with system permissions: it's a system send, as in CRM.
- **Existing templates:** none exist on the live site, so there's no data migration. Templates created before this change are blocked at send time with a clear "add parameter rows" error.
- **Resolution** happens in WhatsApp Message `before_insert` for Outgoing messages with `use_template` set, `template` set and empty `template_parameters`:
  1. For each placeholder present in the template body (`{{n}}`), find its parameter row.
  2. Read the value from the reference doc (`reference_doctype` / `reference_name`), following the link if dotted.
  3. Use `default_value` if the value is empty.
  4. If there's still no value, or there's no row for the placeholder, or there's no reference doc, raise `frappe.ValidationError` listing the unresolved placeholders, e.g. "Template 'Welcome' needs {{2}} (organization) but CRM Lead LEAD-0001 has no value."
  5. Store the resolved values as a JSON list in `template_parameters`, which `relay_client._render_message` already uses.
  6. Values are converted to strings; dates use Frappe's user date format.
- **Template `validate`:**
  - every `{{n}}` in the body needs a parameter row;
  - `field` must exist on `for_doctype` when that is set (with link traversal checked);
  - placeholders are numbered without gaps.
- **Final check at send time:** `relay_client._render_message` raises before sending if any `{{n}}` remains after substitution. The message goes to Failed with reason "unresolved template placeholders".

## 8. Default-number cleanup
- Patch: remove `is_default_outgoing` / `is_default_incoming` from WhatsApp Account, along with the `_enforce_single_default` validation.
- Settings `default_outgoing_account` and `default_incoming_account` stay. Label/help: "Only needed so CRM shows WhatsApp; sending rotates across all Active numbers."
- **Auto-fill:** one shared helper `routing.set_account_active(account)` is used by every path that sets Active (relay callbacks in `api.py`, `connect`, `resume`, already-paired, RateLimited recovery). If `default_outgoing_account` is empty or points at a missing account, it sets it to this account. Same for incoming. Patch: fill both now if empty.

## 9. Node relay removal (final task, controller + user)
1. The bench `Procfile` already runs the Go relay (cut over 2026-10-31), and `CLAUDE.md` is stale on that point; fix it. The controller verifies a bench restart restored the Go relay's sessions: `/info` shows connected sessions with no rescan.
2. Delete `server.js`, `session_manager.js`, `frappe_client.js`, `package.json`, `package-lock.json`, `node_modules/` and `.sessions/` (the old Chromium profiles; ask the user before deleting, as they're local data).
3. Remove the Node sections and browser gotchas from `CLAUDE.md`, keeping the ones that still apply (`.localhost` DNS, `RELAY_PORT` naming, File insert-only, private files, chat-type noise, unknown acks, Ctrl+Z).
4. Drop the Node lines from `.dockerignore`, and the Node `.bak` backups from the bench (`Procfile.node-backup` / `.env.node-backup`) once confirmed. Remove `CHROMIUM_PATH` from `.env`.

## 10. Testing
- **Go:**
  - Startup mode selection (env / claimed / unclaimed).
  - `/claim`: happy path, already claimed, bootstrap token required / wrong / right, bad body.
  - The unclaimed relay answers 503 on protected routes.
  - Rotate: the old key is rejected after rotation, and callbacks use the new key.
  - Callback update.
  - `/info` with and without key, never leaking the key.
  - The outbox poster uses the claimed callback URL and Host.
- **Frappe:**
  - Connect Relay / Rotate / Test / Update Callback with mocked HTTP, including all error branches. The key is saved only on success.
  - Template resolution: Lead/Deal/Customer/Sales Order-style docs (use the DocTypes that exist on the site; create minimal docs in tests), dotted link, default value, missing value blocked, missing row blocked, no reference blocked.
  - Template validate.
  - Render final check.
  - Default auto-fill and the field-removal patch.
- **Live (with the user):**
  - Claim a fresh relay instance on a temp data dir and a spare port, **not** the live one. The live relay stays env-claimed.
  - Send one template to their own number.

## 11. Left to the builder
- Go package placement (`internal/claim` or inside `api` / `store`).
- Panel HTML styling.
- Exact error strings beyond those quoted above.
- Whether `/info`'s version comes from ldflags or `debug.ReadBuildInfo`.
