import requests
import frappe
from frappe.utils import now_datetime

from yourwhatsapp.yourwhatsapp.routing import pick_failover_account

TIMEOUT = 15


def relay_settings():
	settings = frappe.get_single("WhatsApp Settings")
	if not settings.relay_base_url:
		raise RelayNotConfigured("WhatsApp Settings: Relay Base URL is not configured")
	return settings.relay_base_url.rstrip("/"), settings.get_password("relay_api_key")


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
			"attach_url": frappe.utils.get_url(doc.attach) if doc.attach else None,
			"local_message_id": doc.name,
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
