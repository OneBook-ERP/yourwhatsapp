"""Health alerts: bell + email to admins when a WhatsApp number goes down or recovers.

A Notification Log of type Alert never emails by itself (Frappe skips Alert in
its per-user email rule), so the email here is the only one.
"""

import frappe
from frappe import _
from frappe.utils import escape_html, get_url_to_form

ALERT_ROLES = ("System Manager", "WhatsApp Admin")
DOWN_THROTTLE_SECONDS = 15 * 60
MANUAL_SECONDS = 5 * 60
LOG_ONCE_SECONDS = 24 * 60 * 60
DOWN_SENT_SECONDS = 30 * 24 * 60 * 60
KEY_PREFIX = "yourwhatsapp:alert:"
KEY_KINDS = ("down", "down_sent", "manual")
SITE_KEY_ACCOUNT = "_all"  # alert_key() owner for site-wide notes
NO_EMAIL_LOGGED_KIND = "no_email_logged"
NO_RECIPIENTS_LOGGED_KIND = "no_recipients_logged"


def alert_key(account: str, kind: str) -> str:
	return f"{KEY_PREFIX}{account}:{kind}"


def clear_alert_keys(account: str) -> None:
	"""Forget an account's alert state, e.g. when it is deleted and its name is reused."""
	for kind in KEY_KINDS:
		frappe.cache.delete_value(alert_key(account, kind))


def account_status_changed(account: str, old: str | None, new: str, reason: str | None = None) -> None:
	"""Alert on a real transition to or from Active.

	`reason="manual"` (an admin's Pause or Logout) is silent and holds back a
	relay Disconnected for 5 minutes. A down alert is throttled to one per 15
	minutes; a recovery is sent only if a down alert was sent, and is never throttled."""
	if reason == "manual":
		frappe.cache.set_value(alert_key(account, "manual"), 1, expires_in_sec=MANUAL_SECONDS)
		return
	if old == new or "Active" not in (old, new):
		return
	if new == "Active":
		_safely(account, _send_recovery, account, old)
	else:
		_safely(account, _send_down, account, new, reason)


def report_down(account: str, status: str, reason: str) -> None:
	"""A down alert for an account that was already down, when a reason worth
	telling turns up (client outdated); skipped if this outage was alerted."""
	if frappe.cache.get_value(alert_key(account, "down_sent")):
		return
	_safely(account, _send_down, account, status, reason)


def alert_recipients() -> list[str]:
	"""Enabled System Manager / WhatsApp Admin users, and Administrator while enabled.
	Who gets an email as well is decided per address (_user_email)."""
	users = frappe.get_all(
		"Has Role",
		filters={"role": ("in", ALERT_ROLES), "parenttype": "User"},
		pluck="parent",
		distinct=True,
	)
	enabled = frappe.get_all("User", filters={"name": ("in", users), "enabled": 1}, pluck="name") if users else []
	recipients = {user for user in enabled if user not in ("Guest", "Administrator")}
	if _administrator_enabled():
		recipients.add("Administrator")
	return sorted(recipients)


def has_outgoing_email_account() -> bool:
	return bool(frappe.db.exists("Email Account", {"default_outgoing": 1, "enable_outgoing": 1}))


def _administrator_enabled() -> bool:
	return bool(frappe.db.get_value("User", "Administrator", "enabled"))


def _user_email(user: str) -> str | None:
	"""A real address only: the install default admin@example.com would bounce."""
	email = _stored_email(user)
	if not email or email.lower().endswith("@example.com"):
		return None
	return email


def _stored_email(user: str) -> str | None:
	return frappe.db.get_value("User", user, "email")


def _send_down(account: str, new: str, reason: str | None):
	if frappe.cache.get_value(alert_key(account, "manual")):
		return
	if frappe.cache.get_value(alert_key(account, "down")):
		return
	frappe.cache.set_value(alert_key(account, "down"), 1, expires_in_sec=DOWN_THROTTLE_SECONDS)
	frappe.cache.set_value(alert_key(account, "down_sent"), 1, expires_in_sec=DOWN_SENT_SECONDS)
	detail = f"{new} ({reason})" if reason else new
	subject = _("WhatsApp number {0} is down: {1}").format(account, detail)
	_notify(account, subject, _("Messages cannot be sent from this number until it is Active again."))


def _send_recovery(account: str, old: str | None):
	if not frappe.cache.get_value(alert_key(account, "down_sent")):
		return
	frappe.cache.delete_value(alert_key(account, "down_sent"))
	subject = _("WhatsApp number {0} is back online").format(account)
	_notify(account, subject, _("The number was {0} and is Active again.").format(old or _("down")))


def _notify(account: str, subject: str, detail: str):
	recipients = alert_recipients()
	if not recipients:
		_log_once_a_day(
			NO_RECIPIENTS_LOGGED_KIND,
			"WhatsApp health alert has no recipients",
			"No WhatsApp alert recipients — give a user System Manager or WhatsApp Admin.",
		)
		return
	link = get_url_to_form("WhatsApp Account", account)
	for user in recipients:
		frappe.get_doc(
			{
				"doctype": "Notification Log",
				"for_user": user,
				"type": "Alert",
				"subject": subject,
				"email_content": detail,
				"document_type": "WhatsApp Account",
				"document_name": account,
			}
		).insert(ignore_permissions=True)
	_email(recipients, subject, detail, link)


def _email(recipients: list[str], subject: str, detail: str, link: str):
	addresses = [email for email in map(_user_email, recipients) if email]
	if not addresses:
		return
	if not has_outgoing_email_account():
		_log_once_a_day(
			NO_EMAIL_LOGGED_KIND,
			"WhatsApp health alert not emailed",
			"No default outgoing Email Account is enabled, so WhatsApp health alerts only reach the bell.",
		)
		return
	open_label = _("Open the account")
	message = (
		f"<p>{escape_html(subject)}</p><p>{escape_html(detail)}</p>"
		f'<p><a href="{escape_html(link)}">{escape_html(open_label)}</a></p>'
	)
	frappe.sendmail(recipients=addresses, subject=subject, message=message, now=False)


def _log_once_a_day(kind: str, title: str, message: str):
	key = alert_key(SITE_KEY_ACCOUNT, kind)
	if frappe.cache.get_value(key):
		return
	frappe.cache.set_value(key, 1, expires_in_sec=LOG_ONCE_SECONDS)
	frappe.log_error(title=title, message=message)


def _safely(account: str, send, *args):
	try:
		send(*args)
	except Exception:
		# A failing alert must never undo or block the status change that triggered it,
		# not even when the transaction is too broken to write the Error Log.
		try:
			frappe.log_error(
				title="WhatsApp health alert failed", reference_doctype="WhatsApp Account", reference_name=account
			)
		except Exception:
			pass
