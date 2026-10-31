from unittest.mock import patch

import frappe
from frappe.tests import IntegrationTestCase

from yourwhatsapp.yourwhatsapp import routing


class TestOutgoingMessage(IntegrationTestCase):
	def setUp(self):
		# Route only among this test's accounts, without touching the live ones.
		self.test_accounts = set()
		real_eligible = routing.get_eligible_accounts
		for patcher in (
			patch("frappe.enqueue"),
			patch.object(
				routing,
				"get_eligible_accounts",
				side_effect=lambda exclude=None: [
					a for a in real_eligible(exclude=exclude) if a.name in self.test_accounts
				],
			),
		):
			patcher.start()
			self.addCleanup(patcher.stop)

	def tearDown(self):
		frappe.db.rollback()

	def _make_account(self, name, phone_number, **fields):
		self.test_accounts.add(name)
		return frappe.get_doc(
			{
				"doctype": "WhatsApp Account",
				"account_name": name,
				"phone_number": phone_number,
				"status": "Active",
				**fields,
			}
		).insert(ignore_permissions=True)

	def _outgoing(self, to="15551234567", **fields):
		return frappe.get_doc(
			{
				"doctype": "WhatsApp Message",
				"type": "Outgoing",
				"to": to,
				"content_type": "text",
				"message": "hi",
				**fields,
			}
		).insert(ignore_permissions=True)

	# G4: a linked number never sends to itself.

	def test_never_routes_to_recipients_own_account(self):
		own = self._make_account("Own Number", "+91 95663 72605", priority=0)
		other = self._make_account("Other Number", "15550002222", priority=5)
		with patch.object(frappe, "get_cached_value", return_value="Priority"):
			msg = self._outgoing(to="919566372605")
		self.assertEqual(msg.whatsapp_account, other.name)
		self.assertNotEqual(msg.whatsapp_account, own.name)

	def test_only_own_account_left_means_no_account(self):
		self._make_account("Own Number", "919566372605")
		self.assertIsNone(routing.pick_account(recipient="+91 95663-72605"))

	# G5: outgoing numbers are stored as digits only.

	def test_to_normalized_to_digits(self):
		self._make_account("Sender", "15550002222")
		msg = self._outgoing(to="+91 (95663) 72605")
		self.assertEqual(frappe.db.get_value("WhatsApp Message", msg.name, "to"), "919566372605")

	def test_crm_conversation_still_found_after_normalizing(self):
		self._make_account("Sender", "15550002222")
		from crm.api.whatsapp import create_whatsapp_message, get_whatsapp_messages

		lead = frappe.get_doc(
			{"doctype": "CRM Lead", "first_name": "Normalize Test", "mobile_no": "+91 95663 72605"}
		).insert(ignore_permissions=True)
		name = create_whatsapp_message("CRM Lead", lead.name, "hello", "+91 95663 72605", "", "")
		self.assertEqual(frappe.db.get_value("WhatsApp Message", name, "to"), "919566372605")
		self.assertIn(name, [m["name"] for m in get_whatsapp_messages("CRM Lead", lead.name)])

	def test_any_account_links_to_same_crm_lead(self):
		# The thread belongs to the customer, not to the number that carried it.
		account_a = self._make_account("Number A", "15550002222")
		account_b = self._make_account("Number B", "15550003333")
		lead = frappe.get_doc(
			{"doctype": "CRM Lead", "first_name": "Thread Test", "mobile_no": "+91 95663 72605"}
		).insert(ignore_permissions=True)
		incoming = frappe.get_doc(
			{
				"doctype": "WhatsApp Message",
				"type": "Incoming",
				"whatsapp_account": account_a.name,
				"from": "+91 95663 72605",
				"content_type": "text",
				"message": "hello",
			}
		).insert(ignore_permissions=True)
		outgoing = self._outgoing(to="+91 95663-72605", whatsapp_account=account_b.name)
		for msg in (incoming, outgoing):
			row = frappe.db.get_value(
				"WhatsApp Message", msg.name, ["from", "to", "reference_doctype", "reference_name"], as_dict=True
			)
			self.assertEqual(row.get("from") or row.to, "919566372605")
			self.assertEqual((row.reference_doctype, row.reference_name), ("CRM Lead", lead.name))

	def _incoming(self, account, from_number, **fields):
		return frappe.get_doc(
			{
				"doctype": "WhatsApp Message",
				"type": "Incoming",
				"whatsapp_account": account.name,
				"from": from_number,
				"content_type": "text",
				"message": "hello",
				**fields,
			}
		).insert(ignore_permissions=True)

	def _reference(self, msg):
		return frappe.db.get_value("WhatsApp Message", msg.name, ["reference_doctype", "reference_name"])

	def test_non_indian_number_links_to_crm_lead(self):
		# CRM's own hook parses bare digits as Indian; "+<digits>" keeps the country code.
		account = self._make_account("Number A", "15550002222")
		lead = frappe.get_doc(
			{"doctype": "CRM Lead", "first_name": "US Lead", "mobile_no": "+1 415 555 2671"}
		).insert(ignore_permissions=True)
		incoming = self._incoming(account, "14155552671")
		outgoing = self._outgoing(to="+1 (415) 555-2671")
		for msg in (incoming, outgoing):
			self.assertEqual(self._reference(msg), ("CRM Lead", lead.name))

	def test_explicit_reference_kept(self):
		self._make_account("Sender", "15550002222")
		frappe.get_doc(
			{"doctype": "CRM Lead", "first_name": "US Lead", "mobile_no": "+1 415 555 2671"}
		).insert(ignore_permissions=True)
		other = frappe.get_doc({"doctype": "CRM Lead", "first_name": "Other Lead"}).insert(
			ignore_permissions=True
		)
		msg = self._outgoing(
			to="14155552671", reference_doctype="CRM Lead", reference_name=other.name
		)
		self.assertEqual(self._reference(msg), ("CRM Lead", other.name))

	def test_lid_sender_not_resolved_as_phone_number(self):
		account = self._make_account("Number A", "15550002222")
		frappe.get_doc(
			{"doctype": "CRM Lead", "first_name": "US Lead", "mobile_no": "+1 415 555 2671"}
		).insert(ignore_permissions=True)
		msg = self._incoming(account, "14155552671", is_lid_sender=1)
		self.assertEqual(self._reference(msg), (None, None))

	# G6: media messages need their file.

	def test_media_without_attachment_rejected(self):
		self._make_account("Sender", "15550002222")
		for content_type in ("image", "video", "audio", "document"):
			with self.assertRaises(frappe.ValidationError) as ctx:
				self._outgoing(content_type=content_type)
			self.assertIn("attachment", str(ctx.exception))

	def test_media_with_attachment_or_template_allowed(self):
		self._make_account("Sender", "15550002222")
		self._outgoing(content_type="image", attach="/private/files/x.png")
		template = frappe.get_doc(
			{"doctype": "WhatsApp Templates", "template_name": "G6 Template", "template": "Hello"}
		).insert(ignore_permissions=True)
		self._outgoing(content_type="document", use_template=1, template=template.name)

	def test_incoming_media_without_attachment_allowed(self):
		# Inbound media is attached after insert (api._save_inbound_media).
		account = self._make_account("Receiver", "15550002222")
		frappe.get_doc(
			{
				"doctype": "WhatsApp Message",
				"type": "Incoming",
				"whatsapp_account": account.name,
				"from": "15551234567",
				"content_type": "image",
			}
		).insert(ignore_permissions=True)
