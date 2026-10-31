import requests
import frappe
from frappe.model.document import Document
from frappe.utils import cint, random_string

from yourwhatsapp.yourwhatsapp.relay_client import relay_settings

TIMEOUT = 15


class WhatsAppAccount(Document):
	def before_insert(self):
		if not self.session_id:
			self.session_id = f"acc-{random_string(10).lower()}"

	@frappe.whitelist()
	def connect(self):
		self._check_admin()
		return self._relay_post("start", {"proxy": self.proxy or None})

	@frappe.whitelist()
	def pair_with_code(self):
		self._check_admin()
		if not self.phone_number:
			frappe.throw("Set the account's phone number before pairing with a code.")
		result = self._relay_post("pair-code", {"phone": self.phone_number})
		return {"code": result["code"]}

	@frappe.whitelist()
	def pause(self):
		self._check_admin()
		self._relay_post("pause")
		self.db_set("status", "Paused")

	@frappe.whitelist()
	def resume(self):
		self._check_admin()
		self._relay_post("resume", {"proxy": self.proxy or None})
		self.db_set("status", "Disconnected")

	@frappe.whitelist()
	def logout(self):
		self._check_admin()
		self._relay_post("logout")
		self.db_set({"status": "Disconnected", "connected_at": None})

	def _check_admin(self):
		roles = frappe.get_roles()
		if "WhatsApp Admin" not in roles and "System Manager" not in roles:
			frappe.throw("Not permitted to manage a WhatsApp Account.", frappe.PermissionError)

	def _relay_post(self, action, body=None):
		base_url, api_key = relay_settings()
		response = requests.post(
			f"{base_url}/session/{self.session_id}/{action}",
			headers={"x-api-key": api_key},
			json=body,
			timeout=TIMEOUT,
		)
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


def _relay_error_message(response):
	try:
		body = response.json()
	except ValueError:
		body = {}
	detail = body.get("detail") or body.get("error")
	return f"Relay error ({response.status_code}): {detail}" if detail else f"Relay error ({response.status_code})"
