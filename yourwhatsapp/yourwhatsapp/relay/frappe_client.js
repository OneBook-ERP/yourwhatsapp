const axios = require("axios");

const FRAPPE_BASE_URL = process.env.FRAPPE_BASE_URL || "http://whatsapp.localhost:8000";
const RELAY_API_KEY = process.env.RELAY_API_KEY;

const client = axios.create({
	headers: { "X-Api-Key": RELAY_API_KEY },
	timeout: 15000,
});

async function callFrappe(method, payload) {
	const url = `${FRAPPE_BASE_URL}/api/method/yourwhatsapp.yourwhatsapp.api.${method}`;
	try {
		await client.post(url, payload);
	} catch (err) {
		console.error(`[frappe_client] ${method} failed:`, err.response?.data || err.message);
	}
}

module.exports = {
	updateQr: (session_id, qr_base64, status) => callFrappe("update_qr", { session_id, qr_base64, status }),
	inboundMessage: (payload) => callFrappe("inbound_message", payload),
	messageStatus: (message_id, status, error_reason) =>
		callFrappe("message_status", { message_id, status, error_reason }),
};
