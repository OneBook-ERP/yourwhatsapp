import secrets
from contextlib import contextmanager

import frappe
import requests
from frappe import _
from frappe.model.document import Document
from frappe.utils.password import remove_encrypted_password, set_encrypted_password

DOCTYPE = "WhatsApp Settings"
TIMEOUT = 15
KEY_LOCK_TIMEOUT = 10  # seconds to wait for another key change to commit its pending key
ALREADY_CLAIMED = (
	"This relay is already claimed. If it's yours, use Rotate Key with the current key, "
	"or reset the relay's volume."
)
ENV_MODE = (
	"This relay takes its key from its RELAY_API_KEY environment variable. "
	"Type that value into Relay API Key by hand and save."
)
ENV_MODE_CALLBACK = (
	"This relay takes its callback URL from its FRAPPE_BASE_URL environment variable; change it there."
)
KEY_PENDING = "A key change is still pending. Use Retry first so this site and the relay agree on the key."
KEY_CHANGE_BUSY = "Another relay key change is in progress. Try again in a moment."
KEYS_REJECTED = (
	"The relay rejects both the current and the pending key. The pending key is kept. "
	"If this relay is yours, reset the relay's volume and use Retry."
)
# Answers that prove the relay refused this request, once /info confirms it
# doesn't take the pending key either.
REFUSALS = {
	"already_claimed": ALREADY_CLAIMED,
	"bad_bootstrap_token": "The relay rejected the bootstrap token.",
	"env_mode": ENV_MODE,
	"invalid api key": "The relay rejected the current key.",
}


class WhatsAppSettings(Document):
	@frappe.whitelist(methods=["POST"])
	def connect_relay(self):
		frappe.only_for("System Manager")
		self._refuse_if_pending()
		try:
			info = self._relay_info()
		except requests.exceptions.RequestException as exc:
			return _failed(_unreachable(exc), self._known_keys())
		if info.get("mode") == "env":
			return {"ok": False, "mode": "env", "message": ENV_MODE}
		if info.get("claimed"):
			return {"ok": False, "message": ALREADY_CLAIMED}
		key = self._begin_key_change()
		return self._claim(key)

	@frappe.whitelist(methods=["POST"])
	def rotate_key(self):
		frappe.only_for("System Manager")
		current = self._require_current_key()
		key = self._begin_key_change()
		return self._rotate(current, key)

	@frappe.whitelist(methods=["POST"])
	def retry_pending(self):
		"""Finish a claim or rotation whose outcome is unknown, without ever dropping the pending key."""
		frappe.only_for("System Manager")
		pending = self.get_password("relay_pending_key", raise_exception=False)
		if not pending:
			frappe.throw(_("There is no pending key change to retry."))
		try:
			info = self._relay_info(pending)
		except requests.exceptions.RequestException as exc:
			return _failed(_unreachable(exc), self._known_keys())
		if _authorized(info):
			# The earlier claim or rotation landed; only its answer was lost.
			return self._promote_pending()
		if not info.get("claimed"):
			return self._claim(pending)
		current = self._current_key()
		if current and self._accepts(current):
			return self._rotate(current, pending)
		return {"ok": False, "state": "keys_rejected", "message": KEYS_REJECTED}

	@frappe.whitelist(methods=["POST"])
	def update_callback(self):
		frappe.only_for("System Manager")
		current = self._require_current_key()
		body = {"callback_url": self._callback_url(), "site_name": frappe.local.site}
		try:
			response = self._request("PUT", "/claim/callback", key=current, body=body)
		except requests.exceptions.RequestException as exc:
			return _failed(_unreachable(exc), self._known_keys())
		if _ok(response):
			return {"ok": True, "message": _("Callback URL updated.")}
		if _error_code(response) == "env_mode":
			return _failed(ENV_MODE_CALLBACK, self._known_keys())
		return _failed(_relay_error(response), self._known_keys())

	@frappe.whitelist()
	def test_connection(self):
		"""The relay's /info plus a state: ok, unreachable, key_rejected or unclaimed."""
		frappe.only_for("System Manager")
		pending = bool(self.get_password("relay_pending_key", raise_exception=False))
		try:
			response = self._request("GET", "/info", key=self._current_key())
		except requests.exceptions.RequestException as exc:
			return self._unreachable_state(_unreachable(exc), pending)
		if response.status_code == 401:
			return {"state": "key_rejected", "pending": pending}
		if not _ok(response):
			return self._unreachable_state(_relay_error(response), pending)
		info = _json(response)
		return {**info, "state": _info_state(info), "pending": pending}

	def _unreachable_state(self, message, pending):
		return {"state": "unreachable", "pending": pending, "message": _scrub(message, self._known_keys())}

	def _begin_key_change(self):
		"""Commit a new pending key, unless another change already holds one.

		The lock spans check, write and commit, so two admins can't both start
		a change and overwrite each other's pending key."""
		with _key_change_lock():
			if _pending_key_committed():
				frappe.throw(_(KEY_PENDING))
			key = secrets.token_urlsafe(32)
			write_password("relay_pending_key", key)
			frappe.db.commit()
		return key

	def _claim(self, key):
		body = {"api_key": key, "callback_url": self._callback_url(), "site_name": frappe.local.site}
		token = self.get_password("relay_bootstrap_token", raise_exception=False)
		if token:
			body["bootstrap_token"] = token
		return self._send_key_change(key, "POST", "/claim", None, body, _("Relay connected."))

	def _rotate(self, current, key):
		body = {"api_key": key}
		return self._send_key_change(key, "POST", "/claim/rotate", current, body, _("Key rotated."))

	def _send_key_change(self, key, method, path, auth_key, body, success):
		"""Send an already committed pending key; it becomes current only on a 2xx,
		or once the relay is seen to accept it."""
		secrets_shown = [*self._known_keys(), key]
		try:
			response = self._request(method, path, key=auth_key, body=body)
		except requests.exceptions.RequestException as exc:
			return _failed(_unreachable(exc) + " " + _("Use Retry."), secrets_shown)
		if _ok(response):
			self._promote_pending(key)
			return {"ok": True, "message": success}
		if response.status_code >= 500:
			# Outcome unknown (or the relay lost its claim): keep the key for Retry.
			return _failed(_relay_error(response) + " " + _("Use Retry."), secrets_shown)
		if self._accepts(key):
			# Refused now, but an earlier attempt already landed.
			return self._promote_pending(key)
		write_password("relay_pending_key", None)
		return _failed(REFUSALS.get(_error_code(response)) or _relay_error(response), secrets_shown)

	def _promote_pending(self, key=None):
		write_password("relay_api_key", key or self.get_password("relay_pending_key"))
		write_password("relay_pending_key", None)
		return {"ok": True, "message": _("The relay uses the new key; it is saved.")}

	def _accepts(self, key):
		"""Whether the relay authorizes this key: /info shows the keyed detail only then."""
		try:
			return _authorized(self._relay_info(key))
		except requests.exceptions.RequestException:
			return False

	def _relay_info(self, key=None):
		response = self._request("GET", "/info", key=key)
		response.raise_for_status()
		return _json(response)

	def _request(self, method, path, key=None, body=None):
		base_url = (self.relay_base_url or "").strip().rstrip("/")
		if not base_url:
			frappe.throw(_("Set the Relay Base URL first."))
		headers = {"x-api-key": key} if key else {}
		return requests.request(method, f"{base_url}{path}", headers=headers, json=body, timeout=TIMEOUT)

	def _refuse_if_pending(self):
		if self.get_password("relay_pending_key", raise_exception=False):
			frappe.throw(_(KEY_PENDING))

	def _require_current_key(self):
		current = self._current_key()
		if not current:
			frappe.throw(_("There is no relay key yet: use Connect Relay first."))
		return current

	def _current_key(self):
		return self.get_password("relay_api_key", raise_exception=False)

	def _known_keys(self):
		return [self._current_key(), self.get_password("relay_pending_key", raise_exception=False)]

	def _callback_url(self):
		return (self.relay_reachable_frappe_url or "").strip() or frappe.utils.get_url()


def write_password(fieldname, value):
	"""Store a Settings password directly, without saving (and returning) the whole doc.

	The Singles row holds the usual mask so a later form save keeps the secret."""
	if value:
		set_encrypted_password(DOCTYPE, DOCTYPE, value, fieldname)
		frappe.db.set_single_value(DOCTYPE, fieldname, "*" * len(value))
	else:
		remove_encrypted_password(DOCTYPE, DOCTYPE, fieldname)
		frappe.db.set_single_value(DOCTYPE, fieldname, None)


def key_lock_name():
	# MariaDB named locks are server-wide: scope this one to the site's database.
	return f"{frappe.conf.db_name}:whatsapp_relay_key"


@contextmanager
def _key_change_lock():
	acquired = frappe.db.sql("SELECT GET_LOCK(%s, %s)", (key_lock_name(), KEY_LOCK_TIMEOUT))[0][0]
	if acquired != 1:
		frappe.throw(_(KEY_CHANGE_BUSY))
	try:
		yield
	finally:
		frappe.db.sql("SELECT RELEASE_LOCK(%s)", (key_lock_name(),))


def _pending_key_committed():
	# A locking read sees the latest committed row, not this transaction's
	# snapshot, so a pending key another request just committed is not missed.
	return bool(
		frappe.db.sql(
			"SELECT 1 FROM `__Auth` WHERE doctype=%s AND name=%s AND fieldname=%s FOR UPDATE",
			(DOCTYPE, DOCTYPE, "relay_pending_key"),
		)
	)


def _ok(response):
	return 200 <= response.status_code < 300


def _authorized(info):
	return "sessions" in info


def _info_state(info):
	if not info.get("claimed"):
		return "unclaimed"
	# /info answers 200 to a wrong key too, just without the keyed detail.
	return "ok" if _authorized(info) else "key_rejected"


def _failed(message, keys):
	return {"ok": False, "message": _scrub(message, keys)}


def _scrub(message, keys):
	for key in keys:
		if key:
			message = message.replace(key, "****")
	return message


def _unreachable(exc):
	return _("Relay not reachable: {0}.").format(exc)


def _json(response):
	try:
		body = response.json()
	except ValueError:
		return {}
	return body if isinstance(body, dict) else {}


def _error_code(response):
	return _json(response).get("error")


def _relay_error(response):
	body = _json(response)
	detail = body.get("detail") or body.get("error")
	if detail:
		return _("Relay error ({0}): {1}.").format(response.status_code, detail)
	return _("Relay error ({0}).").format(response.status_code)
