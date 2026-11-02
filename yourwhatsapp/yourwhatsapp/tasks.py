import time

import frappe
import requests
from frappe.utils import now_datetime

from yourwhatsapp.yourwhatsapp import alerts
from yourwhatsapp.yourwhatsapp.routing import mark_disconnected, set_account_active

RECONCILE_MINUTES = 5  # hooks.py runs reconcile_account_status on this cron
# Cron runs drift by a few seconds; "down for a job interval" allows for that.
RECONCILE_SLACK_SECONDS = 30
RELAY_DOWN = ("disconnected", "not_started", "logged_out")


def reset_daily_message_counts():
	frappe.db.set_value("WhatsApp Account", {}, "daily_message_count", 0, update_modified=False)


def recover_rate_limited_accounts(names=None):
	"""Lift relay-originated bans once they expire; a RateLimited account without an expiry was set by an admin.
	names limits the run to those accounts (tests)."""
	# "between" rather than "<=": Frappe coalesces NULL to a zero date for "<=", which would match manual bans.
	filters = {"status": "RateLimited", "rate_limited_until": ["between", ["2000-01-01", now_datetime()]]}
	if names is not None:
		filters["name"] = ["in", names]
	for account in frappe.get_all("WhatsApp Account", filters=filters, pluck="name"):
		set_account_active(account, connected_at=False)


def retry_relay_unavailable_messages(names=None):
	"""Re-dispatch messages relay_client parked while the relay was unavailable, once due.
	names limits the run to those messages (tests)."""
	from yourwhatsapp.yourwhatsapp.relay_client import SEND_METHOD

	filters = {
		"type": "Outgoing",
		"status": "Queued",
		# "between" rather than "<=": Frappe coalesces NULL to a zero date for "<=".
		"next_retry_at": ["between", ["2000-01-01", now_datetime()]],
	}
	if names is not None:
		filters["name"] = ["in", names]
	for name in frappe.get_all("WhatsApp Message", filters=filters, pluck="name"):
		# Cleared first so the next tick can't dispatch it twice.
		frappe.db.set_value("WhatsApp Message", name, "next_retry_at", None, update_modified=False)
		frappe.enqueue(SEND_METHOD, queue="long", message_name=name, enqueue_after_commit=True)


def relay_down_key(account: str) -> str:
	return f"yourwhatsapp:relay_down:{account}"


def reconcile_account_status(names=None):
	"""Align each account's status with the relay's view, in case a callback was
	missed. Connected -> Active; down for at least one job interval ->
	Disconnected. Paused and held bans are never overridden (routing guards).
	A relay that can't be reached ends the run with no changes; any other
	failure is logged and skips only that account. names limits the run (tests)."""
	from yourwhatsapp.yourwhatsapp.relay_client import RelayNotConfigured, relay_settings, session_status

	# First: a held-back alert matters most while the relay is down or unreachable.
	alerts.send_pending_down_alerts(names)
	try:
		relay_settings()
	except RelayNotConfigured:
		return
	filters = {"session_id": ["is", "set"]}
	if names is not None:
		filters["name"] = ["in", names]
	for account, session_id, status in frappe.get_all(
		"WhatsApp Account",
		filters=filters,
		fields=["name", "session_id", "status"],
		order_by="creation asc",
		as_list=True,
	):
		try:
			relay_status = session_status(session_id)
			_reconcile(account, status, relay_status)
			frappe.db.set_value(
				"WhatsApp Account", account, "last_health_check", now_datetime(), update_modified=False
			)
		except (requests.exceptions.ConnectionError, requests.exceptions.Timeout) as exc:
			frappe.logger("yourwhatsapp").warning(f"reconcile: relay unreachable, skipping this run: {exc}")
			return
		except Exception:
			frappe.log_error(
				title="WhatsApp status reconcile failed",
				reference_doctype="WhatsApp Account",
				reference_name=account,
			)


def _reconcile(account: str, status: str, relay_status: str | None):
	key = relay_down_key(account)
	if relay_status == "connected":
		frappe.cache.delete_value(key)
		if status != "Active":
			set_account_active(account)
	elif relay_status in RELAY_DOWN:
		first_seen = frappe.cache.get_value(key)
		if first_seen is None:
			# Expires so a mark left by an outage long ago doesn't count for a new one.
			frappe.cache.set_value(key, time.time(), expires_in_sec=(2 * RECONCILE_MINUTES + 1) * 60)
		elif status != "Disconnected" and time.time() - first_seen >= RECONCILE_MINUTES * 60 - RECONCILE_SLACK_SECONDS:
			mark_disconnected(account)
	# Anything else (connecting, qr_pending, paused, temp_banned...) is in flux
	# or reported by its own callback: no change.
