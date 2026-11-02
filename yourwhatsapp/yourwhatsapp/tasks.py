import frappe
import requests
from frappe.utils import now_datetime

from yourwhatsapp.yourwhatsapp.routing import mark_disconnected, set_account_active

RECONCILE_MINUTES = 5  # hooks.py runs reconcile_account_status on this cron
RECONCILE_TIMEOUT = 5
RELAY_DOWN = ("disconnected", "not_started", "logged_out")


def reset_daily_message_counts():
	frappe.db.set_value("WhatsApp Account", {}, "daily_message_count", 0, update_modified=False)


def recover_rate_limited_accounts():
	"""Lift relay-originated bans once they expire; a RateLimited account without an expiry was set by an admin."""
	# "between" rather than "<=": Frappe coalesces NULL to a zero date for "<=", which would match manual bans.
	for account in frappe.get_all(
		"WhatsApp Account",
		filters={"status": "RateLimited", "rate_limited_until": ["between", ["2000-01-01", now_datetime()]]},
		pluck="name",
	):
		set_account_active(account, connected_at=False)


def retry_relay_unavailable_messages():
	"""Re-dispatch messages relay_client parked while the relay was unavailable, once due."""
	from yourwhatsapp.yourwhatsapp.relay_client import SEND_METHOD

	# "between" rather than "<=": Frappe coalesces NULL to a zero date for "<=".
	for name in frappe.get_all(
		"WhatsApp Message",
		filters={
			"type": "Outgoing",
			"status": "Queued",
			"next_retry_at": ["between", ["2000-01-01", now_datetime()]],
		},
		pluck="name",
	):
		# Cleared first so the next tick can't dispatch it twice.
		frappe.db.set_value("WhatsApp Message", name, "next_retry_at", None, update_modified=False)
		frappe.enqueue(SEND_METHOD, queue="long", message_name=name, enqueue_after_commit=True)


def relay_down_key(account: str) -> str:
	return f"yourwhatsapp:relay_down:{account}"


def reconcile_account_status():
	"""Align each account's status with the relay's view, in case a callback was
	missed. Connected -> Active; down on two checks in a row (so for at least one
	job interval) -> Disconnected. Paused and held bans are never overridden
	(routing guards). An account the relay can't answer for is left alone."""
	from yourwhatsapp.yourwhatsapp.relay_client import RelayNotConfigured, relay_settings

	try:
		base_url, api_key = relay_settings()
	except RelayNotConfigured:
		return
	for account, session_id, status in frappe.get_all(
		"WhatsApp Account",
		filters={"session_id": ["is", "set"]},
		fields=["name", "session_id", "status"],
		as_list=True,
	):
		relay_status = _relay_status(base_url, api_key, session_id)
		if relay_status is None:
			continue
		_reconcile(account, status, relay_status)
		frappe.db.set_value("WhatsApp Account", account, "last_health_check", now_datetime(), update_modified=False)


def _relay_status(base_url: str, api_key: str, session_id: str) -> str | None:
	try:
		response = requests.get(
			f"{base_url}/session/{session_id}/status",
			headers={"x-api-key": api_key},
			timeout=RECONCILE_TIMEOUT,
		)
		if not response.ok:
			frappe.logger("yourwhatsapp").warning(f"reconcile: relay status {response.status_code} for {session_id}")
			return None
		return response.json().get("status")
	except (requests.exceptions.RequestException, ValueError) as exc:
		frappe.logger("yourwhatsapp").warning(f"reconcile: relay unreachable for {session_id}: {exc}")
		return None


def _reconcile(account: str, status: str, relay_status: str):
	key = relay_down_key(account)
	if relay_status == "connected":
		frappe.cache.delete_value(key)
		if status != "Active":
			set_account_active(account)
	elif relay_status in RELAY_DOWN:
		if not frappe.cache.get_value(key):
			# Expires so a mark left by a check long ago doesn't count as "still down".
			frappe.cache.set_value(key, 1, expires_in_sec=(2 * RECONCILE_MINUTES + 1) * 60)
		elif status != "Disconnected":
			mark_disconnected(account)
	# Anything else (connecting, qr_pending, paused, temp_banned...) is in flux
	# or reported by its own callback: no change.
