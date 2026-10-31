import requests
import frappe
from frappe import _
from frappe.model.document import Document
from frappe.utils import cint, now_datetime, random_string

from yourwhatsapp.yourwhatsapp.api import relay_may_set_status
from yourwhatsapp.yourwhatsapp.relay_client import relay_settings

TIMEOUT = 15
ALREADY_PAIRED = "already_paired"


class WhatsAppAccount(Document):
	def before_insert(self):
		if not self.session_id:
			self.session_id = f"acc-{random_string(10).lower()}"

	@frappe.whitelist()
	def connect(self):
		self._check_admin()
		result = self._relay_post("start", self._proxy_body(), allow_already_paired=True)
		status = result.get("status")
		if status == ALREADY_PAIRED:
			return self._already_paired()
		if status == "paused":
			frappe.throw("Account is paused on the relay; use Resume")
		if status == "connected" and relay_may_set_status(self.name, "Active"):
			self._set_active()
		return result

	@frappe.whitelist()
	def pair_with_code(self):
		self._check_admin()
		if not self.phone_number:
			frappe.throw("Set the account's phone number before pairing with a code.")
		result = self._relay_post(
			"pair-code", {"phone": self.phone_number, **self._proxy_body()}, allow_already_paired=True
		)
		if result.get("status") == ALREADY_PAIRED:
			return self._already_paired()
		return {"code": result["code"]}

	@frappe.whitelist()
	def pause(self):
		self._check_admin()
		self._relay_post("pause")
		self.db_set("status", "Paused")

	@frappe.whitelist()
	def resume(self):
		self._check_admin()
		result = self._relay_post("resume", self._proxy_body())
		if result.get("status") == "connected":
			self._set_active()
		else:
			self.db_set("status", "Disconnected")

	@frappe.whitelist()
	def logout(self):
		self._check_admin()
		self._relay_post("logout")
		self.db_set({"status": "Disconnected", "connected_at": None})

	def _proxy_body(self):
		# "" clears the relay's stored proxy; null would keep it. A change
		# applies on the next connect (Pause, then Resume, for a live session).
		return {"proxy": self.proxy or ""}

	def _already_paired(self):
		"""The relay already holds a linked device for this account: nothing to pair."""
		if relay_may_set_status(self.name, "Active"):
			self._set_active()
		return {
			"already_paired": True,
			"message": _("This number is already linked; no pairing needed."),
		}

	def _set_active(self):
		self.db_set({"status": "Active", "connected_at": now_datetime()})

	def _check_admin(self):
		roles = frappe.get_roles()
		if "WhatsApp Admin" not in roles and "System Manager" not in roles:
			frappe.throw("Not permitted to manage a WhatsApp Account.", frappe.PermissionError)

	def _relay_post(self, action, body=None, allow_already_paired=False):
		base_url, api_key = relay_settings()
		try:
			response = requests.post(
				f"{base_url}/session/{self.session_id}/{action}",
				headers={"x-api-key": api_key},
				json=body,
				timeout=TIMEOUT,
			)
		except requests.exceptions.RequestException as exc:
			frappe.throw(f"Relay not reachable: {exc}")
		if allow_already_paired and _is_already_paired(response):
			return {"status": ALREADY_PAIRED}
		if response.status_code >= 400:
			frappe.throw(_relay_error_message(response))
		return response.json()

	def validate(self):
		self._enforce_single_default("is_default_incoming")
		self._enforce_single_default("is_default_outgoing")

	def _enforce_single_default(self, fieldname):
		if not cint(self.get(fieldname)):
			return
		others = frappe.get_all(
			"WhatsApp Account",
			filters={fieldname: 1, "name": ["!=", self.name]},
		)
		for other in others:
			frappe.db.set_value("WhatsApp Account", other.name, fieldname, 0)


def _is_already_paired(response):
	# The relay answers 409 "conflict" both for "already paired" and "paused".
	if response.status_code != 409:
		return False
	try:
		return "already paired" in (response.json().get("detail") or "")
	except ValueError:
		return False


def _relay_error_message(response):
	try:
		body = response.json()
	except ValueError:
		body = {}
	detail = body.get("detail") or body.get("error")
	return f"Relay error ({response.status_code}): {detail}" if detail else f"Relay error ({response.status_code})"
