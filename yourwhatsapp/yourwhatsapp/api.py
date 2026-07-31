import frappe
from frappe.utils import now_datetime


def _verify_relay_key():
	expected = frappe.get_single("WhatsApp Settings").get_password("relay_api_key")
	provided = frappe.get_request_header("X-Api-Key")
	if not expected or not provided or provided != expected:
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
):
	_verify_relay_key()

	account = frappe.db.get_value("WhatsApp Account", {"session_id": session_id}, "name")
	if not account:
		frappe.throw(f"No WhatsApp Account found for session {session_id}", frappe.DoesNotExistError)

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
		}
	)
	doc.insert(ignore_permissions=True)
	return {"name": doc.name}


@frappe.whitelist(allow_guest=True, methods=["POST"])
def message_status(message_id: str, status: str, error_reason: str | None = None):
	_verify_relay_key()

	name = frappe.db.get_value("WhatsApp Message", {"message_id": message_id}, "name")
	if not name:
		frappe.throw(f"No WhatsApp Message found for message_id {message_id}", frappe.DoesNotExistError)

	frappe.db.set_value("WhatsApp Message", name, "status", status)
	if error_reason:
		frappe.db.set_value("WhatsApp Message", name, "error_reason", error_reason)
	return {"ok": True}


@frappe.whitelist(allow_guest=True, methods=["POST"])
def update_qr(session_id: str, qr_base64: str | None = None, status: str | None = None):
	_verify_relay_key()

	account = frappe.db.get_value("WhatsApp Account", {"session_id": session_id}, "name")
	if not account:
		frappe.throw(f"No WhatsApp Account found for session {session_id}", frappe.DoesNotExistError)

	if status:
		frappe.db.set_value("WhatsApp Account", account, "status", status)
		if status == "Active":
			frappe.db.set_value("WhatsApp Account", account, "connected_at", now_datetime())

	if qr_base64:
		_save_qr_image(account, qr_base64)

	return {"ok": True}


def _save_qr_image(account: str, qr_base64: str):
	import base64

	file_doc = frappe.get_doc(
		{
			"doctype": "File",
			"file_name": f"{account}-qr.png",
			"attached_to_doctype": "WhatsApp Account",
			"attached_to_name": account,
			"content": base64.b64decode(qr_base64),
			"decode": False,
		}
	)
	file_doc.save(ignore_permissions=True)
	frappe.db.set_value("WhatsApp Account", account, "qr_code", file_doc.file_url)
