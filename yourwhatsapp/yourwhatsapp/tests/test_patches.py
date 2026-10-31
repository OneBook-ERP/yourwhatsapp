import os
import tempfile
import unittest

from yourwhatsapp.patches.v1_0.delete_orphan_public_qr_images import _delete_orphans

ACCOUNTS = {"Primary India Number", "Harris"}


class TestDeleteOrphanPublicQrImages(unittest.TestCase):
	"""Runs against a temp dir only: never the site's real public files."""

	def _run(self, names, referenced=()):
		with tempfile.TemporaryDirectory() as public_dir:
			for name in names:
				with open(os.path.join(public_dir, name), "wb") as f:
					f.write(b"qr")
			deleted = _delete_orphans(public_dir, set(referenced), ACCOUNTS)
			return deleted, set(os.listdir(public_dir))

	def test_deletes_orphaned_account_qr_images(self):
		deleted, left = self._run(["Primary India Number-qre87db8.png", "Harris-qr.png"])
		self.assertEqual((deleted, left), (2, set()))

	def test_keeps_qr_image_with_file_record(self):
		name = "Primary India Number-qre87db8.png"
		deleted, left = self._run([name], referenced={f"/files/{name}"})
		self.assertEqual((deleted, left), (0, {name}))

	def test_keeps_files_that_are_not_account_qr_images(self):
		names = [
			"store-qrcode.png",
			"summer-qr-promo.png",
			"upi-qr.png",  # matches the shape, but "upi" is no WhatsApp Account
			"Harris-qrXYZ.png",
			"Harris-qr.jpg",
		]
		deleted, left = self._run(names)
		self.assertEqual((deleted, left), (0, set(names)))
