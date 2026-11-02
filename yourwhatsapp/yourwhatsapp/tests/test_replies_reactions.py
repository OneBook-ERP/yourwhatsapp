from unittest.mock import patch

import frappe

from yourwhatsapp.yourwhatsapp import api, relay_client, routing
from yourwhatsapp.yourwhatsapp.doctype.whatsapp_settings.whatsapp_settings import write_password
from yourwhatsapp.yourwhatsapp.tests.test_relay_client import RELAY_KEY, relay_response
from yourwhatsapp.yourwhatsapp.tests.utils import LiveSafeTestCase

CUSTOMER = "15557770001"
REPLY_FIELDS = ("reply_to_message_id", "reply_from_me", "reply_text", "reply_participant")


class TestRepliesReactions(LiveSafeTestCase):
	def setUp(self):
		super().setUp()
		settings = frappe.get_single("WhatsApp Settings")
		settings.relay_base_url = "http://relay.test"
		settings.save(ignore_permissions=True)
		write_password("relay_api_key", RELAY_KEY)

		# Route only among this test's accounts, without touching the live ones.
		self.test_accounts = set()
		real_eligible = routing.get_eligible_accounts
		for patcher in (
			patch("frappe.enqueue"),
			patch("frappe.get_request_header", return_value=RELAY_KEY),
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

		self.account_a = self._make_account("Reply Test A", "+1 555 000 1111")
		self.account_b = self._make_account("Reply Test B", "15550002222")

	def _make_account(self, name, phone_number):
		self.test_accounts.add(name)
		return frappe.get_doc(
			{
				"doctype": "WhatsApp Account",
				"account_name": name,
				"phone_number": phone_number,
				"status": "Active",
			}
		).insert(ignore_permissions=True)

	def _incoming(self, message_id, account=None, **fields):
		return frappe.get_doc(
			{
				"doctype": "WhatsApp Message",
				"type": "Incoming",
				"whatsapp_account": (account or self.account_a).name,
				"from": CUSTOMER,
				"content_type": "text",
				"message": "hello from the customer",
				"message_id": message_id,
				**fields,
			}
		).insert(ignore_permissions=True)

	def _outgoing(self, account=None, **fields):
		values = {
			"doctype": "WhatsApp Message",
			"type": "Outgoing",
			"to": CUSTOMER,
			"content_type": "text",
			"message": "hi",
			**fields,
		}
		if account:
			values["whatsapp_account"] = account.name
		return frappe.get_doc(values).insert(ignore_permissions=True)

	def _sent_outgoing(self, message_id, account=None, **fields):
		doc = self._outgoing(account=account or self.account_a, **fields)
		doc.db_set({"status": "Sent", "message_id": message_id})
		return doc

	def _reply(self, target_id, account=None, **fields):
		return self._outgoing(
			account=account or self.account_a, is_reply=1, reply_to_message_id=target_id, message="sure", **fields
		)

	def _reaction(self, target_id, emoji="👍", **fields):
		return self._outgoing(content_type="reaction", message=emoji, reply_to_message_id=target_id, **fields)

	def _payload(self, msg, *responses):
		with patch("requests.post", side_effect=list(responses or [relay_response(202)])) as post:
			relay_client.send_message(msg.name)
		return post.call_args.kwargs["json"], post

	def _row(self, name):
		return frappe.db.get_value(
			"WhatsApp Message", name, ["status", "whatsapp_account", "error_reason"], as_dict=True
		)

	# Quoted replies

	def test_reply_payload_from_incoming_target(self):
		self._incoming("IN-1")
		body, _ = self._payload(self._reply("IN-1"))
		self.assertEqual(body["reply_to_message_id"], "IN-1")
		self.assertIs(body["reply_from_me"], False)
		self.assertEqual(body["reply_text"], "hello from the customer")
		self.assertNotIn("reply_participant", body)

	def test_reply_payload_from_outgoing_target(self):
		self._sent_outgoing("OUT-1", message="our price list")
		body, _ = self._payload(self._reply("OUT-1"))
		self.assertEqual(body["reply_to_message_id"], "OUT-1")
		self.assertIs(body["reply_from_me"], True)
		self.assertEqual(body["reply_text"], "our price list")
		self.assertEqual(body["reply_participant"], "15550001111")

	def test_reply_media_target_placeholder(self):
		self._incoming("IN-IMG", content_type="image", message="")
		self._incoming("IN-VID", content_type="video", message="look at this")
		# CRM stores an uncaptioned outgoing attachment's URL as its message.
		url = "/private/files/catalogue.pdf"
		self._sent_outgoing("OUT-DOC", content_type="document", attach=url, message=url)

		body, _ = self._payload(self._reply("IN-IMG"))
		self.assertEqual(body["reply_text"], "[image]")
		body, _ = self._payload(self._reply("IN-VID"))
		self.assertEqual(body["reply_text"], "look at this")
		body, _ = self._payload(self._reply("OUT-DOC"))
		self.assertEqual(body["reply_text"], "[document]")

	def test_reply_quote_across_numbers(self):
		# Rotation is unchanged: B replies, quoting what A sent.
		self._sent_outgoing("OUT-A", account=self.account_a, message="sent from A")
		body, _ = self._payload(self._reply("OUT-A", account=self.account_b))
		self.assertEqual(body["session_id"], self.account_b.session_id)
		self.assertEqual(body["reply_participant"], "15550001111")
		self.assertEqual(body["reply_text"], "sent from A")

	def test_reply_missing_target_plain(self):
		msg = self._reply("NOT-IN-FRAPPE")
		body, _ = self._payload(msg)
		for field in REPLY_FIELDS:
			self.assertNotIn(field, body)
		self.assertEqual(body["message"], "sure")
		self.assertEqual(self._row(msg.name).status, "Queued")

	# Reactions

	def test_reaction_pinned_to_target_account(self):
		self._incoming("IN-R", account=self.account_a)
		reaction = self._reaction("IN-R", whatsapp_account=self.account_b.name)
		self.assertEqual(reaction.whatsapp_account, self.account_a.name)
		body, _ = self._payload(reaction)
		self.assertEqual(body["session_id"], self.account_a.session_id)
		self.assertEqual(body["content_type"], "reaction")
		self.assertEqual(body["message"], "👍")
		self.assertEqual(body["reply_to_message_id"], "IN-R")
		self.assertIs(body["reply_from_me"], False)

	def test_reaction_inactive_account_refused_at_insert(self):
		from crm.api.whatsapp import react_on_whatsapp_message

		target = self._incoming("IN-OFF", account=self.account_a)
		frappe.db.set_value("WhatsApp Account", self.account_a.name, "status", "Disconnected")
		before = frappe.db.count("WhatsApp Message", {"content_type": "reaction"})
		with self.assertRaises(frappe.ValidationError) as ctx:
			react_on_whatsapp_message("👍", target.name)
		self.assertIn(f"Reaction needs {self.account_a.name} to be connected", str(ctx.exception))
		self.assertEqual(frappe.db.count("WhatsApp Message", {"content_type": "reaction"}), before)

	def test_reaction_unknown_target_refused(self):
		with self.assertRaises(frappe.ValidationError):
			self._reaction("NOT-IN-FRAPPE")

	def test_reaction_no_failover(self):
		self._incoming("IN-F", account=self.account_a)

		# Synchronous: a 409 fails the reaction instead of trying B.
		sync = self._reaction("IN-F")
		_, post = self._payload(sync, relay_response(409, {"error": "session_unavailable"}))
		self.assertEqual(post.call_count, 1)
		row = self._row(sync.name)
		self.assertEqual((row.status, row.whatsapp_account), ("Failed", self.account_a.name))

		# Asynchronous: a send_failed callback fails it, with no failover job.
		frappe.db.set_value("WhatsApp Account", self.account_a.name, "status", "Active")
		late = self._reaction("IN-F")
		self._payload(late)
		frappe.enqueue.reset_mock()
		api.message_status(status="Failed", local_message_id=late.name, error_code="send_failed", error_reason="boom")
		failovers = [c for c in frappe.enqueue.call_args_list if c.args and c.args[0] == api.FAILOVER_METHOD]
		self.assertEqual(failovers, [])
		row = self._row(late.name)
		self.assertEqual((row.status, row.whatsapp_account), ("Failed", self.account_a.name))

	def test_reaction_not_counted(self):
		self._incoming("IN-C", account=self.account_a)
		reaction = self._reaction("IN-C")
		api.message_status(status="Sent", local_message_id=reaction.name, message_id="WA-REACT")
		row = self._row(reaction.name)
		self.assertEqual((row.status, row.whatsapp_account), ("Sent", self.account_a.name))
		self.assertEqual(frappe.db.get_value("WhatsApp Account", self.account_a.name, "daily_message_count"), 0)

	# Inbound replies in CRM

	def test_inbound_reply_visible_in_crm(self):
		from crm.api.whatsapp import get_whatsapp_messages

		lead = frappe.get_doc({"doctype": "CRM Lead", "first_name": "Reply Test Lead"}).insert(
			ignore_permissions=True
		)
		found = (lead.name, "CRM Lead")
		with (
			patch("crm.integrations.api.get_contact_lead_or_deal_from_number", return_value=found),
			patch("crm.api.whatsapp.get_contact_lead_or_deal_from_number", return_value=found),
		):
			target = self._sent_outgoing("OUT-Q", message="our price list")
			api.inbound_message(
				session_id=self.account_a.session_id,
				from_number=CUSTOMER,
				message="is it still valid?",
				message_id="IN-Q",
				is_reply=True,
				reply_to_message_id="OUT-Q",
			)

		messages = get_whatsapp_messages("CRM Lead", lead.name)
		reply = next(m for m in messages if m["message_id"] == "IN-Q")
		self.assertTrue(reply["is_reply"])
		self.assertEqual(reply["reply_to"], target.name)
		self.assertEqual(reply["reply_to_type"], "Outgoing")
		self.assertEqual(reply["reply_message"], "our price list")
