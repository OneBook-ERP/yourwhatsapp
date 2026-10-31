from datetime import datetime
from zoneinfo import ZoneInfo

import frappe
from frappe.utils import get_system_timezone, now_datetime


def get_eligible_accounts(exclude=None):
	filters = {"status": "Active"}
	accounts = frappe.get_all(
		"WhatsApp Account",
		filters=filters,
		fields=["name", "priority", "last_used", "daily_message_count", "daily_limit"],
	)
	exclude = exclude or []
	return [a for a in accounts if a.name not in exclude and a.daily_message_count < a.daily_limit]


def pick_account(exclude=None):
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


def mark_rate_limited(account, until=None):
	"""Flag an account RateLimited unless an admin has Paused it: Frappe owns admin intent."""
	if frappe.db.get_value("WhatsApp Account", account, "status") == "Paused":
		return
	values = {"status": "RateLimited"}
	if until:
		values["rate_limited_until"] = until
	frappe.db.set_value("WhatsApp Account", account, values)


def to_system_datetime(rfc3339: str) -> datetime:
	"""RFC3339 from the relay -> naive datetime in the system timezone, as Datetime fields expect."""
	parsed = datetime.fromisoformat(rfc3339.replace("Z", "+00:00"))
	if parsed.tzinfo is None:
		return parsed
	return parsed.astimezone(ZoneInfo(get_system_timezone())).replace(tzinfo=None)
