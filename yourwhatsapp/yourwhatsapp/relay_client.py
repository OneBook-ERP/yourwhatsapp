import json
import re
import time

import frappe
import requests

from yourwhatsapp.yourwhatsapp.routing import mark_rate_limited, pick_account, to_system_datetime

DISPATCH_BUDGET_SECONDS = 240
TIMEOUT = 30  # the relay only queues the send, so a slow answer means trouble


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
		for i, param in enumerate(json.loads(doc.template_parameters), start=1):
			text = text.replace("{{" + str(i) + "}}", str(param))
	return text


def send_message(message_name: str):
	doc = frappe.get_doc("WhatsApp Message", message_name)
	if doc.type != "Outgoing" or doc.status not in ("Queued", "Failed"):
		return

	if not doc.whatsapp_account:
		_fail(doc, "No eligible WhatsApp Account for routing")
		return

	_dispatch(doc)


def handle_async_failure(message_name: str, error_code: str):
	"""Re-dispatch a message the relay accepted but then failed to send (api.message_status enqueues this)."""
	# Row lock: attempted_accounts is read-modify-written below.
	doc = frappe.get_doc("WhatsApp Message", message_name, for_update=True)
	if doc.type != "Outgoing" or doc.status != "Failed":
		return

	_remember_attempt(doc, doc.whatsapp_account)
	# message_status only applies a Failed callback while the message is Queued.
	doc.db_set("status", "Queued")
	if _switch_account(doc, f"Relay error: {error_code}"):
		_dispatch(doc)


def increment_daily_count(account: str):
	# Atomic in SQL: concurrent Sent callbacks must not lose increments.
	frappe.db.sql(
		"UPDATE `tabWhatsApp Account` SET daily_message_count = daily_message_count + 1 WHERE name = %s",
		account,
	)


def _fail(doc, reason: str):
	doc.db_set("status", "Failed")
	doc.db_set("error_reason", reason)


def _attempted_accounts(doc) -> list[str]:
	return json.loads(doc.attempted_accounts or "[]")


def _remember_attempt(doc, account: str):
	attempted = _attempted_accounts(doc)
	if account and account not in attempted:
		attempted.append(account)
		doc.db_set("attempted_accounts", json.dumps(attempted))


def _switch_account(doc, reason: str) -> bool:
	"""Move the message to an account not yet tried; Failed with `reason` when none is left."""
	next_account = pick_account(exclude=_attempted_accounts(doc))
	if not next_account:
		_fail(doc, f"{reason}; no other WhatsApp Account available")
		return False
	doc.db_set("whatsapp_account", next_account)
	return True


def _to_is_lid(doc) -> bool:
	# A reply to a sender whose number WhatsApp only exposed as a LID must go back out to that LID.
	return bool(
		frappe.db.get_value(
			"WhatsApp Message",
			{"type": "Incoming", "from": doc.to},
			"is_lid_sender",
			order_by="creation desc",
		)
	)


def _post(doc, account, base_url: str, api_key: str):
	"""POST to the relay; a timeout is retried once on the same account (relay idempotency answers `duplicate`)."""
	payload = {
		"session_id": account.session_id,
		"to": doc.to,
		"to_is_lid": _to_is_lid(doc),
		"content_type": doc.content_type,
		"message": _render_message(doc),
		"local_message_id": doc.name,
		**_attach_payload(doc),
	}
	for attempt in (1, 2):
		try:
			return requests.post(
				f"{base_url}/message/send", json=payload, headers={"x-api-key": api_key}, timeout=TIMEOUT
			)
		except requests.exceptions.Timeout:
			if attempt == 2:
				raise


def _error_code(response) -> str | None:
	try:
		return response.json().get("error")
	except Exception:
		return None


def _ban_expiry(response):
	"""The relay's 429 detail ends with 'temporarily banned until <RFC3339>'."""
	try:
		detail = response.json().get("detail") or ""
		match = re.search(r"\d{4}-\d\d-\d\dT[\d:.]+(?:Z|[+-]\d\d:\d\d)", detail)
		return to_system_datetime(match.group(0)) if match else None
	except Exception:
		return None


def _dispatch(doc):
	started = time.monotonic()
	try:
		base_url, api_key = relay_settings()
	except RelayNotConfigured as e:
		_fail(doc, str(e))
		return

	previous_account = None
	while True:
		if previous_account and time.monotonic() - started > DISPATCH_BUDGET_SECONDS:
			_fail(doc, "relay unreachable; dispatch time budget exhausted")
			return
		account = frappe.get_doc("WhatsApp Account", doc.whatsapp_account)
		_remember_attempt(doc, account.name)

		try:
			response = _post(doc, account, base_url, api_key)
		except requests.exceptions.RequestException as e:
			previous_account = account.name
			if not _switch_account(doc, f"Relay unreachable: {e}"):
				return
			continue

		status = response.status_code
		if status in (200, 202):
			# 202: queued, the Sent/Failed verdict arrives via message_status.
			# 200 duplicate: the relay already has this message; nothing to do.
			if status == 200 and previous_account:
				# The earlier account (e.g. after a timeout) actually holds the send.
				doc.db_set("whatsapp_account", previous_account)
			return
		code = _error_code(response)
		if status in (400, 422):
			_fail(doc, f"Relay rejected message: {code or status}")
			return
		if status == 429:
			mark_rate_limited(account.name, _ban_expiry(response))
		previous_account = account.name
		if not _switch_account(doc, f"Relay error: {code or status}"):
			return
