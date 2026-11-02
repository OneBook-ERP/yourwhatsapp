import json

import frappe
from frappe import _
from frappe.model.document import Document

from yourwhatsapp.yourwhatsapp.routing import normalize_number, pick_account
from yourwhatsapp.yourwhatsapp.templating import resolve

MEDIA_CONTENT_TYPES = ("image", "video", "audio", "document")


class WhatsAppMessage(Document):
	def before_insert(self):
		if self.type == "Outgoing":
			# Digits only (as inbound `from` below): threads are keyed by the
			# customer's number, whichever of our accounts carried the message.
			self.to = normalize_number(self.to) or self.to
			if self.content_type == "reaction":
				self._pin_reaction_account()
			else:
				self._prepare_outgoing()
			self.status = "Queued"
		else:
			self.set("from", normalize_number(self.get("from")) or self.get("from"))
			self.status = "Received"

	def validate(self):
		self._link_crm_reference()

	def _prepare_outgoing(self):
		self._validate_media_attached()
		self.message_type = "Template" if self.use_template else "Manual"
		# Before resolution: a template reads from the matched Lead/Deal.
		self._link_crm_reference()
		self._fill_template_parameters()
		if not self.whatsapp_account:
			self.whatsapp_account = pick_account(recipient=self.to)

	def _pin_reaction_account(self):
		"""The one exception to rotation: WhatsApp only applies a reaction sent
		from the number whose chat holds the target, so it never fails over."""
		self.message_type = "Manual"
		target = reply_target(self.reply_to_message_id)
		account = target and target.whatsapp_account
		if not account or frappe.db.get_value("WhatsApp Account", account, "status") != "Active":
			frappe.throw(
				_("Reaction needs {0} to be connected").format(account or _("the original WhatsApp Account")),
				frappe.ValidationError,
			)
		self.whatsapp_account = account

	def _fill_template_parameters(self):
		if not (self.use_template and self.template) or self.template_parameters:
			return
		values = resolve(self.template, self.reference_doctype, self.reference_name)
		if values:
			self.template_parameters = json.dumps(values)

	def _link_crm_reference(self):
		"""Link the customer's CRM Lead/Deal/Contact from the stored digits.

		CRM's own validate hook parses bare digits with India as the default
		country, so a non-Indian number never matches; "+<digits>" keeps its
		country code. A LID is not a phone number and is left alone.
		"""
		if self.reference_doctype and self.reference_name:
			return
		number = self.get("from") if self.type == "Incoming" else self.to
		if not number or not number.isdigit() or self._is_lid():
			return
		try:
			from crm.integrations.api import get_contact_lead_or_deal_from_number
		except ImportError:
			return
		name, doctype = get_contact_lead_or_deal_from_number(f"+{number}")
		if doctype and name:
			self.reference_doctype, self.reference_name = doctype, name

	def _is_lid(self):
		if self.type == "Incoming":
			return bool(self.is_lid_sender)
		return bool(
			frappe.db.exists("WhatsApp Message", {"type": "Incoming", "from": self.to, "is_lid_sender": 1})
		)

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


def reply_target(message_id: str | None):
	"""The WhatsApp Message a reply quotes or a reaction targets, by WhatsApp's message id."""
	if not message_id:
		return None
	return frappe.db.get_value(
		"WhatsApp Message",
		{"message_id": message_id},
		["type", "message", "content_type", "attach", "whatsapp_account"],
		as_dict=True,
		order_by="creation desc",
	)
