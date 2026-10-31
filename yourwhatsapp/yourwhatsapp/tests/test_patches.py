import os
from contextlib import redirect_stdout
from io import StringIO

import frappe
from frappe.tests import IntegrationTestCase
from frappe.utils import random_string

from yourwhatsapp.patches.v1_0 import delete_orphan_public_qr_images


class TestDeleteOrphanPublicQrImages(IntegrationTestCase):
	def tearDown(self):
		frappe.db.rollback()

	def _public_file(self, file_name):
		path = frappe.get_site_path("public", "files", file_name)
		with open(path, "wb") as f:
			f.write(b"qr")
		self.addCleanup(lambda: os.path.exists(path) and os.remove(path))
		return path

	def test_deletes_only_orphaned_qr_images(self):
		tag = random_string(8).lower()
		orphan = self._public_file(f"g8-{tag}-qr.png")
		suffixed = self._public_file(f"g8-{tag}-qr5f2a1.png")
		unrelated = self._public_file(f"g8-{tag}-photo.png")
		kept_doc = frappe.get_doc(
			{
				"doctype": "File",
				"file_name": f"g8-{tag}-kept-qr.png",
				"content": b"qr",
				"decode": False,
				"is_private": 0,
			}
		).insert(ignore_permissions=True)
		kept = frappe.get_site_path("public", kept_doc.file_url.lstrip("/"))
		self.addCleanup(lambda: os.path.exists(kept) and os.remove(kept))

		out = StringIO()
		with redirect_stdout(out):
			delete_orphan_public_qr_images.execute()

		self.assertFalse(os.path.exists(orphan))
		self.assertFalse(os.path.exists(suffixed))
		self.assertTrue(os.path.exists(unrelated))
		self.assertTrue(os.path.exists(kept))
		self.assertRegex(out.getvalue(), r"Deleted \d+ orphan public QR image")
