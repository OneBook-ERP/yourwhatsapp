import requests
import frappe
from frappe.utils import now_datetime

from yourwhatsapp.yourwhatsapp.routing import pick_failover_account

TIMEOUT = 45  # media uploads through a loaded headless browser can take longer than a plain text send


def relay_settings():
	settings = frappe.get_single("WhatsApp Settings")
	if not settings.relay_base_url:
		raise RelayNotConfigured("WhatsApp Settings: Relay Base URL is not configured")
	return settings.relay_base_url.rstrip("/"), settings.get_password("relay_api_key")


def _attach_payload(doc):
	if not doc.attach:
		return {}

	# Read and base64-encode the file directly instead of handing the relay a
	# URL to fetch: private files (the CRM/Frappe default for user uploads)
	# aren't reachable by an anonymous HTTP GET from the relay's Node process,
	# and even public-file URLs hit the .localhost-DNS-resolution gotcha. This
	# sidesteps both — the relay never needs network access to Frappe at all
	# for the attachment itself, only bytes we hand it directly.
	import mimetypes

	file_doc = frappe.get_doc("File", {"file_url": doc.attach})
	content = file_doc.get_content()
	if isinstance(content, str):
		content = content.encode("utf-8")

	import base64

	mimetype = mimetypes.guess_type(file_doc.file_name)[0] or "application/octet-stream"
	return {
		"attach_base64": base64.b64encode(content).decode("ascii"),
		"attach_mimetype": mimetype,
		"attach_filename": file_doc.file_name,
	}


class RelayNotConfigured(Exception):
	pass


def _render_message(doc):
	if not doc.use_template or not doc.template:
		return doc.message

	template_doc = frappe.get_doc("WhatsApp Templates", doc.template)
	text = template_doc.template
	if doc.template_parameters:
		import json

		for i, param in enumerate(json.loads(doc.template_parameters), start=1):
			text = text.replace("{{" + str(i) + "}}", str(param))
	return text


def send_message(message_name: str):
	doc = frappe.get_doc("WhatsApp Message", message_name)
	if doc.type != "Outgoing" or doc.status not in ("Queued", "Failed"):
		return

	if not doc.whatsapp_account:
		doc.db_set("status", "Failed")
		doc.db_set("error_reason", "No eligible WhatsApp Account for routing")
		return

	_dispatch(doc)


def _dispatch(doc, attempted_accounts=None):
	attempted_accounts = attempted_accounts or []
	account = frappe.get_doc("WhatsApp Account", doc.whatsapp_account)

	try:
		base_url, api_key = relay_settings()
		payload = {
			"session_id": account.session_id,
			"to": doc.to,
			"content_type": doc.content_type,
			"message": _render_message(doc),
			"local_message_id": doc.name,
			**_attach_payload(doc),
		}
		response = requests.post(
			f"{base_url}/message/send",
			json=payload,
			headers={"x-api-key": api_key},
			timeout=TIMEOUT,
		)
		response.raise_for_status()
		result = response.json()

		doc.db_set("status", "Sent")
		doc.db_set("message_id", result.get("message_id"))
		frappe.db.set_value(
			"WhatsApp Account",
			account.name,
			"daily_message_count",
			(account.daily_message_count or 0) + 1,
		)
		return
	except RelayNotConfigured as e:
		doc.db_set("status", "Failed")
		doc.db_set("error_reason", str(e))
	except Exception as e:
		if isinstance(e, requests.exceptions.RequestException) and getattr(e.response, "status_code", None) == 429:
			frappe.db.set_value("WhatsApp Account", account.name, "status", "RateLimited")

		attempted_accounts.append(account.name)
		fallback_account = pick_failover_account(account.name)
		if fallback_account and fallback_account not in attempted_accounts:
			doc.db_set("whatsapp_account", fallback_account)
			return _dispatch(doc, attempted_accounts)

		doc.db_set("status", "Failed")
		doc.db_set("error_reason", str(e))
