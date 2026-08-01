frappe.ui.form.on("WhatsApp Account", {
	refresh(frm) {
		if (frm.is_new()) return;

		frm.add_custom_button(__("Connect / Generate QR"), () => {
			frm.call("connect").then(() => {
				frappe.show_alert({ message: __("Starting session — waiting for QR..."), indicator: "blue" });
				poll_until_connected(frm);
			});
		});

		if (frm.doc.status === "Disconnected" && frm.doc.qr_code && !frm._qr_poll_active) {
			poll_until_connected(frm);
		}
	},
});

function poll_until_connected(frm) {
	if (frm._qr_poll_active) return;

	// QR codes rotate every ~20-30s until scanned; keep reloading so the
	// (cache-busted) QR image field stays current instead of showing a stale,
	// expired code that the user can never successfully scan.
	frm._qr_poll_active = true;
	let attempts = 0;
	const MAX_ATTEMPTS = 20;

	const interval = setInterval(() => {
		attempts++;
		if (attempts > MAX_ATTEMPTS || frm.is_dirty() || frm.doc.status !== "Disconnected") {
			clearInterval(interval);
			frm._qr_poll_active = false;
			return;
		}
		frm.reload_doc();
	}, 6000);
}
