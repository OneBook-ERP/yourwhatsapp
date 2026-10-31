import hmac
import frappe
from frappe.utils import get_datetime, now_datetime

from yourwhatsapp.yourwhatsapp.relay_client import increment_daily_count
from yourwhatsapp.yourwhatsapp.routing import mark_rate_limited, to_system_datetime

STATUS_RANK = {"Queued": 0, "Sent": 1, "Delivered": 2, "Read": 3}
# relay_restarted goes through the same job, which retries it on the same account.
FAILOVER_ERROR_CODES = {"session_unavailable", "send_failed", "temp_banned", "relay_restarted"}
FAILOVER_METHOD = "yourwhatsapp.yourwhatsapp.relay_client.handle_async_failure"


def _verify_relay_key():
	expected = frappe.get_single("WhatsApp Settings").get_password("relay_api_key")
	provided = frappe.get_request_header("X-Api-Key")
	if not expected or not provided or not hmac.compare_digest(provided.encode(), expected.encode()):
		frappe.throw("Invalid relay API key", frappe.PermissionError)


@frappe.whitelist(allow_guest=True, methods=["POST"])
def inbound_message(
	session_id: str,
	from_number: str,
	content_type: str = "text",
	message: str = "",
	message_id: str | None = None,
	attach_url: str | None = None,
	is_reply: bool = False,
	reply_to_message_id: str | None = None,
	media_base64: str | None = None,
	media_mimetype: str | None = None,
	media_filename: str | None = None,
	from_lid: bool = False,
):
	_verify_relay_key()

	account = frappe.db.get_value("WhatsApp Account", {"session_id": session_id}, "name")
	if not account:
		frappe.throw(f"No WhatsApp Account found for session {session_id}", frappe.DoesNotExistError)

	if message_id:
		# The relay's outbox retries until it sees a 2xx, so a redelivery of an
		# inbound callback must not create a second message.
		existing = frappe.db.get_value("WhatsApp Message", {"message_id": message_id, "type": "Incoming"})
		if existing:
			return {"name": existing}

	doc = frappe.get_doc(
		{
			"doctype": "WhatsApp Message",
			"type": "Incoming",
			"whatsapp_account": account,
			"from": from_number,
			"content_type": content_type,
			"message": message,
			"message_id": message_id,
			"attach": attach_url,
			"is_reply": is_reply,
			"reply_to_message_id": reply_to_message_id,
			"is_lid_sender": 1 if frappe.utils.cint(from_lid) else 0,
		}
	)
	doc.insert(ignore_permissions=True)

	if media_base64:
		_save_inbound_media_or_note(doc, media_base64, media_mimetype, media_filename)

	return {"name": doc.name}


def _save_inbound_media_or_note(doc, media_base64: str, mimetype: str | None, filename: str | None):
	"""Keep the message even when its media can't be stored (e.g. over the site's max file size)."""
	savepoint = "inbound_media"
	frappe.db.savepoint(savepoint)
	try:
		_save_inbound_media(doc.name, media_base64, mimetype, filename)
	except Exception:
		frappe.db.rollback(save_point=savepoint)
		frappe.log_error(title="WhatsApp inbound media not saved", reference_doctype=doc.doctype, reference_name=doc.name)
		note = "[attachment could not be saved]"
		frappe.db.set_value(doc.doctype, doc.name, "message", f"{doc.message}\n{note}" if doc.message else note)


def _save_inbound_media(message_name: str, media_base64: str, mimetype: str | None, filename: str | None):
	import base64
	import mimetypes

	content = base64.b64decode(media_base64)
	if not filename:
		ext = mimetypes.guess_extension(mimetype or "") or ""
		filename = f"{message_name}{ext}"

	file_doc = frappe.get_doc(
		{
			"doctype": "File",
			"file_name": filename,
			"attached_to_doctype": "WhatsApp Message",
			"attached_to_name": message_name,
			"content": content,
			"decode": False,
			# Customer-sent media (IDs, invoices, photos) must not be readable by
			# anyone holding the URL — private files are permission-checked.
			"is_private": 1,
		}
	)
	file_doc.save(ignore_permissions=True)
	frappe.db.set_value("WhatsApp Message", message_name, "attach", file_doc.file_url)


@frappe.whitelist(allow_guest=True, methods=["POST"])
def message_status(
	status: str,
	message_id: str | None = None,
	error_reason: str | None = None,
	local_message_id: str | None = None,
	error_code: str | None = None,
):
	_verify_relay_key()

	name = _find_message(message_id, local_message_id)
	if not name:
		# Expected, not an error: WhatsApp acks every outgoing message on the
		# linked number, including ones typed on the phone that CRM never sent.
		return {"ok": False, "reason": "unknown message_id"}

	current = frappe.db.get_value("WhatsApp Message", name, "status")
	if status == "Failed":
		return _apply_failure(name, error_reason, error_code, message_id)

	if status not in STATUS_RANK:
		return {"ok": False, "reason": "unknown status"}
	# Receipts can arrive out of order; never move a message backwards.
	if STATUS_RANK[status] < STATUS_RANK.get(current, -1):
		return {"ok": False, "reason": "stale status"}

	values = {"status": status}
	if status == "Sent" and message_id:
		values["message_id"] = message_id
	frappe.db.set_value("WhatsApp Message", name, values)
	if status == "Sent" and current != "Sent":
		account = frappe.db.get_value("WhatsApp Message", name, "whatsapp_account")
		if account:
			increment_daily_count(account)
	return {"ok": True}


def _find_message(message_id: str | None, local_message_id: str | None) -> str | None:
	if local_message_id:
		return frappe.db.exists("WhatsApp Message", local_message_id)
	if message_id:
		# Receipts only ever concern messages we sent; an incoming message can share the id.
		return frappe.db.get_value("WhatsApp Message", {"message_id": message_id, "type": "Outgoing"}, "name")
	return None


def _apply_failure(name: str, error_reason: str | None, error_code: str | None, message_id: str | None):
	# Only a message still waiting on the relay can fail; once it was Sent (or a
	# failure was already handled) a late Failed callback is noise. Re-read under
	# a row lock so concurrent callbacks and the failover job see one another.
	current = frappe.db.get_value("WhatsApp Message", name, "status", for_update=True)
	if current != "Queued":
		return {"ok": False, "reason": "not failable"}

	values = {"status": "Failed"}
	if error_reason:
		values["error_reason"] = error_reason
	if message_id:
		# send_unconfirmed: WhatsApp may still have delivered it, and a later
		# receipt for this id lifts the status (it is not a failover code).
		values["message_id"] = message_id
	frappe.db.set_value("WhatsApp Message", name, values)

	if error_code in FAILOVER_ERROR_CODES:
		if error_code == "temp_banned":
			account = frappe.db.get_value("WhatsApp Message", name, "whatsapp_account")
			if account:
				mark_rate_limited(account)
		frappe.enqueue(FAILOVER_METHOD, message_name=name, error_code=error_code, enqueue_after_commit=True)
	return {"ok": True}


@frappe.whitelist(allow_guest=True, methods=["POST"])
def account_event(session_id: str, event: str, until: str | None = None):
	_verify_relay_key()

	account = frappe.db.get_value("WhatsApp Account", {"session_id": session_id}, "name")
	if not account:
		frappe.throw(f"No WhatsApp Account found for session {session_id}", frappe.DoesNotExistError)

	if event != "temp_banned":
		return {"ok": False, "reason": "unknown event"}

	mark_rate_limited(account, to_system_datetime(until) if until else None)
	return {"ok": True}


@frappe.whitelist(allow_guest=True, methods=["POST"])
def update_qr(session_id: str, qr_base64: str | None = None, status: str | None = None):
	_verify_relay_key()

	account = frappe.db.get_value("WhatsApp Account", {"session_id": session_id}, "name")
	if not account:
		frappe.throw(f"No WhatsApp Account found for session {session_id}", frappe.DoesNotExistError)

	if status and relay_may_set_status(account, status):
		frappe.db.set_value("WhatsApp Account", account, "status", status)
		if status == "Active":
			frappe.db.set_value("WhatsApp Account", account, "connected_at", now_datetime())
			frappe.db.set_value("WhatsApp Account", account, "rate_limited_until", None)
			# A scanned QR is spent, and a leftover one is only a liability.
			_delete_qr_image(account)
			frappe.db.set_value("WhatsApp Account", account, "qr_code", None)

	if qr_base64:
		_save_qr_image(account, qr_base64)

	return {"ok": True}


def relay_may_set_status(account: str, status: str) -> bool:
	"""Frappe owns admin intent (Paused, manual RateLimited); the relay owns connection facts."""
	current, until = frappe.db.get_value("WhatsApp Account", account, ["status", "rate_limited_until"])
	if current == "Paused" and status in ("Active", "Disconnected"):
		return False
	if current == "RateLimited" and status == "Active":
		# Only a relay-originated ban (has an expiry) lifts itself, once it has passed.
		return bool(until) and get_datetime(until) <= now_datetime()
	return True


def qr_file_name(account: str) -> str:
	return f"{account}-qr.png"


def _delete_qr_image(account: str):
	existing = frappe.db.get_value(
		"File",
		{
			"attached_to_doctype": "WhatsApp Account",
			"attached_to_name": account,
			"file_name": qr_file_name(account),
		},
	)
	if not existing:
		return
	try:
		frappe.delete_doc("File", existing, ignore_permissions=True, delete_permanently=True)
	except Exception:
		# Transient lock from a near-simultaneous QR event — the caller falls
		# through and inserts anyway; Frappe will auto-suffix the filename this
		# one time rather than dropping the update entirely.
		pass


def _save_qr_image(account: str, qr_base64: str):
	import base64

	content = base64.b64decode(qr_base64)

	# QR codes rotate every ~20-30s until scanned. Frappe's File doctype only
	# writes the physical file on insert (before_insert), never on update — so
	# updating an existing File doc's `content` and saving silently leaves the
	# old bytes on disk. Delete the previous File first so the reinsert keeps
	# the same filename (no collision, no auto-suffix) instead of piling up.
	_delete_qr_image(account)

	file_doc = frappe.get_doc(
		{
			"doctype": "File",
			"file_name": qr_file_name(account),
			"attached_to_doctype": "WhatsApp Account",
			"attached_to_name": account,
			"content": content,
			"decode": False,
			# A live QR is a credential: whoever scans it links their own device
			# to this number. Public files are served to anyone who guesses the
			# URL, and the filename is derived from the account name.
			"is_private": 1,
		}
	)
	file_doc.save(ignore_permissions=True)

	# Cache-bust: the file URL is otherwise identical across QR rotations (same
	# reused File record), so browsers serve the stale cached image instead of
	# re-fetching the updated content.
	frappe.db.set_value(
		"WhatsApp Account", account, "qr_code", f"{file_doc.file_url}?t={int(frappe.utils.now_datetime().timestamp())}"
	)
