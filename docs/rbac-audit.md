# RBAC audit (2026-11-02)

Audit only: **no permissions were changed.** Source of truth is the code: the doctype JSON `permissions`, the `@frappe.whitelist` methods in `yourwhatsapp/yourwhatsapp/`, and CRM's `crm/api/whatsapp.py`. Re-run this audit when a doctype's permissions or a whitelisted method changes.

## 1. Doctype permissions

`r` read, `w` write, `c` create, `d` delete. A dash means no row for that role (no access). Roles not listed (Guest, All, Desk User, Sales User on Account/Settings, and so on) get nothing from these rows.

| Role | WhatsApp Message | WhatsApp Account | WhatsApp Settings (single) | WhatsApp Templates | WhatsApp Template Parameter |
|---|---|---|---|---|---|
| System Manager | r w c d | r w c d | r w | r w c d | child table |
| WhatsApp Admin | r w c d | r w c d | r w (relay fields guarded, see 3) | r w c d | child table |
| WhatsApp Operator | r w c | r | - | r | child table |
| Sales Manager | r w c d | - | r | r w c | child table |
| Sales User | r w c | - | - | r | child table |

`WhatsApp Template Parameter` is a child table (`istable`) of WhatsApp Templates and has no permission rows of its own: it follows its parent.

None of the doctypes use user permissions, `if_owner`, a permission query or a `has_permission` hook. Anyone who can read WhatsApp Message can read **every** message, whatever Lead or Deal it is linked to.

## 2. Whitelisted methods

### Called by people (Desk or REST)

| Method | Guard | Who can run it |
|---|---|---|
| `WhatsApp Account.connect`, `pair_with_code`, `pause`, `resume`, `logout` | Frappe first requires read on that Account (`run_doc_method` loads the doc with `check_permission`), then `_check_admin()` requires WhatsApp Admin or System Manager | WhatsApp Admin, System Manager. WhatsApp Operator reaches the method (it can read) and is refused by `_check_admin`. |
| `WhatsApp Settings.connect_relay`, `rotate_key`, `retry_pending`, `update_callback`, `replay_dead_callbacks`, `test_connection` | `frappe.only_for("System Manager")` | System Manager only. WhatsApp Admin and Sales Manager are refused. |
| `WhatsApp Settings.validate` (any save) | If a relay connection field changed (`relay_base_url`, `relay_api_key`, `relay_pending_key`, `relay_bootstrap_token`) and the user is not System Manager: `PermissionError` | A WhatsApp Admin can save Settings but not touch those four fields. |

### Called by the relay (`allow_guest=True`, POST only)

`api.ping`, `inbound_message`, `message_status`, `account_event`, `update_qr`. Each starts with `_verify_relay_key()`, a constant-time compare (`hmac.compare_digest`) of the `X-Api-Key` header against `relay_api_key` or `relay_pending_key`. No key or a wrong key raises `PermissionError`. The key is the only credential: anyone holding it can inject inbound messages, forge delivery statuses, change an account's status and push QR images. Keep it out of logs and chat, and rotate it (Settings, Rotate Key) if it leaks.

### Templates

`templating.resolve` runs as the sending user and calls `_check_read` on the Lead/Deal and on every linked document a placeholder reads. If the user cannot read the document the send is blocked ("You don't have permission to read ... so a template can't use it"). Administrator is exempt. This stops a template from copying data the sender could not open.

### CRM's whitelisted WhatsApp API (`crm.api.whatsapp`)

| Method | Guard |
|---|---|
| `create_whatsapp_message`, `send_whatsapp_template`, `react_on_whatsapp_message` | `validate_access`: user must hold System Manager, Sales Manager or Sales User, and (when a reference is given) have read permission on the Lead/Deal. For replies and reactions it also checks read on the referenced WhatsApp Message and on its reference document. Then it inserts with **`ignore_permissions=True`**. |
| `get_whatsapp_messages` | `validate_access` on the reference document |
| `is_whatsapp_enabled`, `is_whatsapp_installed` | none beyond login |

WhatsApp Admin and WhatsApp Operator are **not** in CRM's `ALLOWED_WHATSAPP_ROLES`, so they cannot use the CRM chat unless they also hold a Sales role.

## 3. Risks

1. **Sales User, Sales Manager and WhatsApp Operator can create and write WhatsApp Message directly over REST or Desk** (`/api/resource/WhatsApp Message`), skipping every check in `crm.api.whatsapp`. They can send to any number with no Lead/Deal reference, choose `whatsapp_account` themselves (bypassing rotation and the daily cap choice), and attach any file path they can name. CRM's `validate_access` only protects the CRM endpoints.
2. **Write on an existing message lets those roles edit history**: status, text, `to`, `reply_to_message_id` and `whatsapp_account` of any message, including other agents' and incoming ones. There is no audit beyond Frappe's version log.
3. **Read is global.** Any Sales User sees every customer conversation through REST or the list view, even for Leads and Deals they cannot open in CRM, because there is no user permission or permission query. CRM's per-document check applies only to its own endpoints.
4. **Delete** on WhatsApp Message is held by Sales Manager (and the admin roles), so a Sales Manager can remove conversation records.
5. **WhatsApp Admin can write Settings non-relay fields** (default accounts and anything else not in the four guarded fields), and can pause, resume and log out any account. Logout unlinks a number and forces a re-pair, so it is an availability risk if the role is handed out loosely. It cannot change the relay URL or keys.
6. **Sales Manager authors templates** (r w c, no delete). A template reads fields from the Lead/Deal; the read-permission check at resolve time protects against leaking fields the *sender* cannot read, but a Sales Manager can still write a template that exposes any field they themselves can read to every recipient.
7. **Sales Manager can read Settings.** Password fields are masked on read and never returned, but the relay URL and callback URL are visible to that role.
8. **WhatsApp Operator** (read on Account, plus write on Message) sees every number and its status; Sales roles cannot read Accounts at all, so they see `whatsapp_account` only as a link name.
9. **ignore_permissions inserts in CRM** mean a message is created even if the user has no create permission on WhatsApp Message. That is intended (it is how CRM sends), and it is safe only because `validate_access` runs first. Any new CRM-side caller must keep that order.
10. **The relay callbacks are the only unauthenticated surface** (guest-allowed). The single control is the API key (see 2); there is no IP allowlist or rate limit in Frappe. In Docker the relay and Frappe share an internal network, but the callback URL is reachable through the site's nginx.
11. **Typed `relay_api_key` in env mode** is accepted from a System Manager on save. That is by design; it is the only path where a key passes through the form.

## 4. Recommendations (not applied)

Pick these up when the user decides to change permissions.

1. Remove create/write/delete from **Sales User** and **WhatsApp Operator** on WhatsApp Message (read only). All sends already go through `crm.api.whatsapp`, which uses `ignore_permissions`, so CRM keeps working. Remove delete from Sales Manager too.
2. Add a permission query (or User Permissions on the reference Lead/Deal) so a user reads only messages whose reference document they can read, matching what CRM already enforces.
3. Make `whatsapp_account`, `status`, `message_id`, `type` and `to` read-only for non-admin roles, or add a `validate` rule that rejects a manual insert by a non-admin that has no CRM reference.
4. Limit logout and pause to System Manager if WhatsApp Admin is handed to many people, or give logout its own confirmation.
5. Decide whether Sales Manager should author templates; if not, drop create/write there.
6. Restrict `/api/method/yourwhatsapp.yourwhatsapp.api.*` at nginx to the relay's address, in addition to the key.
7. Add WhatsApp Admin and WhatsApp Operator to CRM's `ALLOWED_WHATSAPP_ROLES` only if they should chat; today they cannot (CRM is installed unmodified, so this means an upstream change).
8. Enable document versioning ("Track Changes") on WhatsApp Message and Account for an edit trail.
