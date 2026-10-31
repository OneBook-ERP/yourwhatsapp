const PAIRING_INSTRUCTIONS = __(
	"On the phone: WhatsApp → Linked Devices → Link a device → Link with phone number instead, then enter this code."
);

frappe.ui.form.on("WhatsApp Account", {
	refresh(frm) {
		if (frm.is_new()) return;

		add_connect_button(frm);
		add_session_buttons(frm);

		if (frm.doc.status === "Disconnected" && frm.doc.qr_code && !frm._qr_poll_active) {
			poll_until_connected(frm);
		}
	},
});

function add_connect_button(frm) {
	if (frm.doc.pairing_method === "Phone Number") {
		frm.add_custom_button(__("Connect / Pair with Code"), () => pair_with_code(frm));
		return;
	}
	frm.add_custom_button(__("Connect / Generate QR"), () => {
		frm.call("connect").then(() => {
			frappe.show_alert({ message: __("Starting session — waiting for QR..."), indicator: "blue" });
			poll_until_connected(frm);
		});
	});
}

function add_session_buttons(frm) {
	if (frm.doc.status === "Paused") {
		frm.add_custom_button(__("Resume"), () => frm.call("resume").then(() => frm.reload_doc()));
	} else {
		frm.add_custom_button(__("Pause"), () => frm.call("pause").then(() => frm.reload_doc()));
	}
	frm.add_custom_button(__("Logout"), () => {
		frappe.confirm(__("Log this account out of WhatsApp? It will have to be paired again."), () =>
			frm.call("logout").then(() => frm.reload_doc())
		);
	});
}

function pair_with_code(frm) {
	// The relay needs a started session before it can issue a pairing code.
	frm.call("connect")
		.then(() => frm.call("pair_with_code"))
		.then((r) => show_pairing_dialog(frm, r.message.code));
}

function show_pairing_dialog(frm, code) {
	const dialog = new frappe.ui.Dialog({
		title: __("Pair with phone number"),
		fields: [{ fieldtype: "HTML", fieldname: "code_html" }],
		on_hide: () => (frm._pairing_dialog_open = false),
	});
	dialog.get_field("code_html").$wrapper.html(
		`<div style="font-family: monospace; font-size: 2rem; letter-spacing: 0.15em; text-align: center; margin: 1rem 0;">${frappe.utils.escape_html(code)}</div>
		<p class="text-muted">${frappe.utils.escape_html(PAIRING_INSTRUCTIONS)}</p>`
	);
	frm._pairing_dialog_open = true;
	dialog.show();
	poll_until_connected(frm, () => dialog.hide());
}

function poll_until_connected(frm, on_connected) {
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
			if (on_connected && frm.doc.status === "Active") on_connected();
			return;
		}
		frm.reload_doc();
	}, 6000);
}
