const STATE_LABELS = {
	ok: { label: __("Connected"), color: "green" },
	unreachable: { label: __("Unreachable"), color: "red" },
	key_rejected: { label: __("Key rejected"), color: "red" },
	unclaimed: { label: __("Unclaimed"), color: "orange" },
};

frappe.ui.form.on("WhatsApp Settings", {
	refresh(frm) {
		// The key is set by the buttons; typed by hand only for an env-mode relay.
		frm.set_df_property("relay_api_key", "read_only", 1);
		if (!frappe.user.has_role("System Manager")) return;

		if (!frm.doc.relay_base_url) {
			render_panel(frm, null);
			return;
		}
		frm.add_custom_button(__("Test Connection"), () => test_connection(frm), __("Relay"));
		test_connection(frm);
	},
});

function test_connection(frm) {
	frm.call({ method: "test_connection", doc: frm.doc }).then((r) => {
		const info = r.message || {};
		frm.set_df_property("relay_api_key", "read_only", info.mode === "env" ? 0 : 1);
		render_panel(frm, info);
		add_relay_buttons(frm, info);
	});
}

function add_relay_buttons(frm, info) {
	const group = __("Relay");
	["Connect Relay", "Rotate Key", "Update Callback URL", "Retry Pending Key"].forEach((label) =>
		frm.remove_custom_button(__(label), group)
	);

	if (info.pending) {
		frm.add_custom_button(__("Retry Pending Key"), () => run(frm, "retry_pending"), group);
		return;
	}
	if (info.state === "unclaimed") {
		frm.add_custom_button(__("Connect Relay"), () => run(frm, "connect_relay"), group);
	}
	if (info.state === "ok" && info.mode === "claimed") {
		frm.add_custom_button(
			__("Rotate Key"),
			() =>
				frappe.confirm(__("Generate a new relay key and send it to the relay?"), () =>
					run(frm, "rotate_key")
				),
			group
		);
		frm.add_custom_button(__("Update Callback URL"), () => run(frm, "update_callback"), group);
	}
}

// Save first: the server reads the saved settings, and a stale form would be
// refused (and could otherwise overwrite the key the server just stored).
function run(frm, method) {
	const saved = frm.is_dirty() ? frm.save() : Promise.resolve();
	saved
		.then(() => frm.call({ method, doc: frm.doc, freeze: true }))
		.then((r) => {
			const result = r.message || {};
			frappe.msgprint({
				title: __("Relay"),
				message: frappe.utils.escape_html(result.message || ""),
				indicator: result.ok ? "green" : "orange",
			});
			return frm.reload_doc();
		});
}

function render_panel(frm, info) {
	const wrapper = frm.get_field("relay_status_html").$wrapper;
	if (!info) {
		wrapper.html(`<p class="text-muted">${__("Set the Relay Base URL and save to see the relay status.")}</p>`);
		return;
	}
	const state = STATE_LABELS[info.state] || { label: info.state, color: "gray" };
	const rows = [
		[__("Status"), `<span class="indicator-pill ${state.color}">${esc(state.label)}</span>`],
		[__("Mode"), esc(info.mode)],
		[__("Version"), esc(info.version)],
	];
	if (info.sessions) {
		rows.push([__("Numbers connected / linked"), `${esc(info.sessions.connected)} / ${esc(info.sessions.total)}`]);
	}
	if (info.outbox) {
		rows.push([__("Callbacks pending / dead"), `${esc(info.outbox.pending)} / ${esc(info.outbox.dead)}`]);
	}
	if (info.callback_url) rows.push([__("Callback URL"), esc(info.callback_url)]);
	if (info.site_name) rows.push([__("Site name"), esc(info.site_name)]);
	if (info.message) rows.push([__("Detail"), esc(info.message)]);

	const notes = [];
	if (info.pending) {
		notes.push(
			__("A key change was sent but not confirmed. Use Relay → Retry Pending Key before anything else.")
		);
	}
	if (info.mode === "env") {
		notes.push(__("This relay takes its key from RELAY_API_KEY: type that value into Relay API Key and save."));
	}

	wrapper.html(`
		<table class="table table-bordered" style="max-width: 640px;">
			${rows.map(([k, v]) => `<tr><th style="width: 40%;">${esc(k)}</th><td>${v || ""}</td></tr>`).join("")}
		</table>
		${notes.map((n) => `<p class="text-warning">${esc(n)}</p>`).join("")}
	`);
}

function esc(value) {
	return value === undefined || value === null ? "" : frappe.utils.escape_html(String(value));
}
