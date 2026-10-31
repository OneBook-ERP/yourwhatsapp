import frappe
from frappe import _
from frappe.model.document import Document

from yourwhatsapp.yourwhatsapp.routing import normalize_number, pick_account

MEDIA_CONTENT_TYPES = ("image", "video", "audio", "document")


class WhatsAppMessage(Document):
	def before_insert(self):
		if self.type == "Outgoing":
			self._validate_media_attached()
			# Digits only, so the recipient matches inbound `from` and account numbers.
			self.to = normalize_number(self.to) or self.to
			self.message_type = "Template" if self.use_template else "Manual"
			if not self.whatsapp_account:
				self.whatsapp_account = pick_account(recipient=self.to)
			self.status = "Queued"
		else:
			self.status = "Received"

	def _validate_media_attached(self):
		if self.content_type in MEDIA_CONTENT_TYPES and not self.attach and not self.use_template:
			frappe.throw(
				_("A {0} message needs an attachment.").format(self.content_type), frappe.ValidationError
			)

	def after_insert(self):
		if self.type == "Outgoing":
			frappe.enqueue(
				"yourwhatsapp.yourwhatsapp.relay_client.send_message",
				queue="long",
				message_name=self.name,
				enqueue_after_commit=True,
			)
