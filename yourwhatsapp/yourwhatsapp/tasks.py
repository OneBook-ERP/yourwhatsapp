import frappe
from frappe.utils import now_datetime


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
		frappe.db.set_value(
			"WhatsApp Account", account, {"status": "Active", "rate_limited_until": None}
		)


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
