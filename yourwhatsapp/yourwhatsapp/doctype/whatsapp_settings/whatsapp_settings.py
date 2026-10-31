import secrets

import frappe
import requests
from frappe import _
from frappe.model.document import Document
from frappe.utils.password import remove_encrypted_password, set_encrypted_password

DOCTYPE = "WhatsApp Settings"
TIMEOUT = 15
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
# Answers that prove the relay did not take the key: no point keeping it for Retry.
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
		return self._claim(_new_pending_key())

	@frappe.whitelist(methods=["POST"])
	def rotate_key(self):
		frappe.only_for("System Manager")
		self._refuse_if_pending()
		current = self._require_current_key()
		return self._rotate(current, _new_pending_key())

	@frappe.whitelist(methods=["POST"])
	def retry_pending(self):
		"""Re-send the claim or rotation for a key the relay may or may not have taken."""
		frappe.only_for("System Manager")
		pending = self.get_password("relay_pending_key", raise_exception=False)
		if not pending:
			frappe.throw(_("There is no pending key change to retry."))
		try:
			info = self._relay_info()
		except requests.exceptions.RequestException as exc:
			return _failed(_unreachable(exc), self._known_keys())
		if not info.get("claimed"):
			return self._claim(pending)
		# A claim or rotation whose 200 was lost: the relay already uses the
		# pending key and answers this repeat with a 200 no-op.
		return self._rotate(self._current_key(), pending)

	@frappe.whitelist(methods=["POST"])
	def update_callback(self):
		frappe.only_for("System Manager")
		current = self._require_current_key()
		body = {"callback_url": self._callback_url(), "site_name": frappe.local.site}
		try:
			response = self._request("PUT", "/claim/callback", key=current, body=body)
		except requests.exceptions.RequestException as exc:
			return _failed(_unreachable(exc), self._known_keys())
		if response.status_code == 200:
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
		if response.status_code != 200:
			return self._unreachable_state(_relay_error(response), pending)
		info = _json(response)
		return {**info, "state": _info_state(info), "pending": pending}

	def _unreachable_state(self, message, pending):
		return {"state": "unreachable", "pending": pending, "message": _scrub(message, self._known_keys())}

	def _claim(self, key):
		body = {"api_key": key, "callback_url": self._callback_url(), "site_name": frappe.local.site}
		token = self.get_password("relay_bootstrap_token", raise_exception=False)
		if token:
			body["bootstrap_token"] = token
		return self._change_key(key, "POST", "/claim", None, body, _("Relay connected."))

	def _rotate(self, current, key):
		return self._change_key(key, "POST", "/claim/rotate", current, {"api_key": key}, _("Key rotated."))

	def _change_key(self, key, method, path, auth_key, body, success):
		"""Never lose a key: it is committed as pending before the relay sees it,
		and becomes the current key only once the relay answers 200."""
		write_password("relay_pending_key", key)
		frappe.db.commit()
		secrets_shown = self._known_keys() + [key]
		try:
			response = self._request(method, path, key=auth_key, body=body)
		except requests.exceptions.RequestException as exc:
			return _failed(_unreachable(exc) + " " + _("Use Retry."), secrets_shown)
		if response.status_code == 200:
			write_password("relay_api_key", key)
			write_password("relay_pending_key", None)
			return {"ok": True, "message": success}
		if response.status_code >= 500:
			# Outcome unknown (or the relay lost its claim): keep the key for Retry.
			return _failed(_relay_error(response) + " " + _("Use Retry."), secrets_shown)
		write_password("relay_pending_key", None)
		return _failed(REFUSALS.get(_error_code(response)) or _relay_error(response), secrets_shown)

	def _relay_info(self):
		response = self._request("GET", "/info")
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


def _new_pending_key():
	return secrets.token_urlsafe(32)


def _info_state(info):
	if not info.get("claimed"):
		return "unclaimed"
	# /info answers 200 to a wrong key too, just without the keyed detail.
	return "ok" if "sessions" in info else "key_rejected"


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
