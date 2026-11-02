import re
from datetime import datetime
from zoneinfo import ZoneInfo

import frappe
from frappe.utils import get_system_timezone, now_datetime

from yourwhatsapp.yourwhatsapp import alerts


def get_eligible_accounts(exclude=None):
	filters = {"status": "Active"}
	accounts = frappe.get_all(
		"WhatsApp Account",
		filters=filters,
		fields=["name", "priority", "last_used", "daily_message_count", "daily_limit"],
	)
	exclude = exclude or []
	return [a for a in accounts if a.name not in exclude and a.daily_message_count < a.daily_limit]


def normalize_number(number: str | None) -> str:
	"""Digits only: '+91 (95663) 72605' -> '919566372605'. LIDs are digits already."""
	return re.sub(r"\D", "", number or "")


def accounts_with_number(number: str | None) -> list[str]:
	"""Accounts whose own linked number is `number` (compared as digits)."""
	digits = normalize_number(number)
	if not digits:
		return []
	accounts = frappe.get_all("WhatsApp Account", fields=["name", "phone_number"])
	return [a.name for a in accounts if normalize_number(a.phone_number) == digits]


def pick_account(exclude=None, recipient=None):
	"""Rotate over eligible accounts; never the recipient's own linked number."""
	exclude = [*(exclude or []), *accounts_with_number(recipient)]
	eligible = get_eligible_accounts(exclude=exclude)
	if not eligible:
		return None

	strategy = frappe.get_cached_value("WhatsApp Settings", "WhatsApp Settings", "routing_strategy") or "Round Robin"

	if strategy == "Priority":
		eligible.sort(key=lambda a: a.priority or 0)
	else:
		eligible.sort(key=lambda a: a.last_used or datetime.min)

	chosen = eligible[0]
	frappe.db.set_value("WhatsApp Account", chosen.name, "last_used", now_datetime())
	return chosen.name


def pick_failover_account(failed_account):
	return pick_account(exclude=[failed_account])


def mark_rate_limited(account, until=None, reason=None) -> bool:
	"""Flag an account RateLimited unless an admin has Paused it: Frappe owns admin intent."""
	old = frappe.db.get_value("WhatsApp Account", account, "status")
	if old == "Paused":
		return False
	values = {"status": "RateLimited"}
	if until:
		values["rate_limited_until"] = until
	frappe.db.set_value("WhatsApp Account", account, values)
	repoint_default_accounts()
	alerts.account_status_changed(account, old, "RateLimited", reason)
	return True


def set_account_active(account, connected_at=True):
	"""Mark an account Active unless an admin hold stops the relay from doing so.

	Every path that activates an account goes through here, so the Settings
	defaults CRM needs (it shows WhatsApp only while the default outgoing
	account is Active) fill themselves.
	"""
	from yourwhatsapp.yourwhatsapp.api import relay_may_set_status

	if not relay_may_set_status(account, "Active"):
		return False
	old = frappe.db.get_value("WhatsApp Account", account, "status")
	values = {"status": "Active", "rate_limited_until": None}
	if connected_at:
		values["connected_at"] = now_datetime()
	frappe.db.set_value("WhatsApp Account", account, values)
	fill_default_accounts(account)
	alerts.account_status_changed(account, old, "Active", None)
	return True


DEFAULT_FIELDS = ("default_outgoing_account", "default_incoming_account")


def fill_default_accounts(account=None, exclude=None):
	"""Point each Settings default that is empty, dangling or not Active at an
	Active account: `account` when it is Active, else the first Active one by
	priority. With no Active account left the defaults stay as they are.
	Sending never reads them; CRM needs the outgoing one Active to show WhatsApp."""
	for fieldname in DEFAULT_FIELDS:
		# Uncached: an earlier write in this transaction bypasses the value cache.
		current = frappe.db.get_single_value("WhatsApp Settings", fieldname, cache=False)
		if current and current != exclude and _is_active(current):
			continue
		replacement = account if account and account != exclude and _is_active(account) else None
		replacement = replacement or _first_active_account(exclude)
		if replacement and replacement != current:
			# A background write: must not bump modified under an open Settings form.
			frappe.db.set_single_value("WhatsApp Settings", fieldname, replacement, update_modified=False)


def repoint_default_accounts(exclude=None):
	"""Call after an account stops being Active (or, with `exclude`, before it is deleted)."""
	fill_default_accounts(exclude=exclude)


def _is_active(account):
	return frappe.db.get_value("WhatsApp Account", account, "status") == "Active"


def _first_active_account(exclude=None):
	# Sorted here: frappe.get_all refuses ifnull() in order_by, and NULL priority means 0.
	accounts = frappe.get_all("WhatsApp Account", filters={"status": "Active"}, fields=["name", "priority", "creation"])
	accounts = sorted((a for a in accounts if a.name != exclude), key=lambda a: (a.priority or 0, a.creation))
	return accounts[0].name if accounts else None


def mark_disconnected(account, reason=None) -> bool:
	"""Record that the relay has no live session for the account, unless an admin Paused it."""
	from yourwhatsapp.yourwhatsapp.api import relay_may_set_status

	if not relay_may_set_status(account, "Disconnected"):
		return False
	old = frappe.db.get_value("WhatsApp Account", account, "status")
	frappe.db.set_value("WhatsApp Account", account, "status", "Disconnected")
	repoint_default_accounts()
	alerts.account_status_changed(account, old, "Disconnected", reason)
	return True


def to_system_datetime(rfc3339: str) -> datetime:
	"""RFC3339 from the relay -> naive datetime in the system timezone, as Datetime fields expect."""
	parsed = datetime.fromisoformat(rfc3339.replace("Z", "+00:00"))
	if parsed.tzinfo is None:
		return parsed
	return parsed.astimezone(ZoneInfo(get_system_timezone())).replace(tzinfo=None)
