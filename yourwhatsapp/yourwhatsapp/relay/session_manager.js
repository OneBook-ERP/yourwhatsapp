const path = require("path");
const qrcode = require("qrcode");
const { Client, LocalAuth, MessageMedia } = require("whatsapp-web.js");
const frappeClient = require("./frappe_client");

const SESSIONS_DIR = path.join(__dirname, ".sessions");

// session_id -> { client, status }
const sessions = new Map();

// WhatsApp increasingly addresses contacts by a privacy-preserving "lid" (Linked ID)
// instead of their real phone number JID. Resolve it back to a phone number when possible.
async function resolvePhoneNumber(client, waId) {
	if (!waId.endsWith("@lid")) {
		return waId.replace("@c.us", "");
	}
	try {
		const [resolved] = await client.getContactLidAndPhone([waId]);
		if (resolved?.pn) {
			return resolved.pn.replace("@c.us", "");
		}
	} catch (err) {
		console.error(`[relay] LID resolution failed for ${waId}:`, err.message);
	}
	return waId.replace("@lid", "");
}

function contentTypeFromMessage(msg) {
	if (msg.type === "chat") return "text";
	if (["image", "video", "audio", "ptt", "document"].includes(msg.type)) {
		return msg.type === "ptt" ? "audio" : msg.type;
	}
	return "text";
}

function buildClient(sessionId) {
	const client = new Client({
		authStrategy: new LocalAuth({ clientId: sessionId, dataPath: SESSIONS_DIR }),
		puppeteer: {
			args: ["--no-sandbox", "--disable-setuid-sandbox"],
			executablePath: process.env.CHROMIUM_PATH || undefined,
		},
	});

	client.on("qr", async (qr) => {
		const dataUrl = await qrcode.toDataURL(qr);
		const base64 = dataUrl.replace(/^data:image\/png;base64,/, "");
		sessions.get(sessionId).status = "qr_pending";
		await frappeClient.updateQr(sessionId, base64, null);
	});

	client.on("ready", () => markConnected(sessionId));

	client.on("disconnected", async () => {
		sessions.get(sessionId).status = "disconnected";
		await frappeClient.updateQr(sessionId, null, "Disconnected");
	});

	client.on("auth_failure", async () => {
		sessions.get(sessionId).status = "disconnected";
		await frappeClient.updateQr(sessionId, null, "Disconnected");
	});

	client.on("message", async (msg) => {
		if (msg.fromMe) return;
		await frappeClient.inboundMessage({
			session_id: sessionId,
			from_number: await resolvePhoneNumber(client, msg.from),
			content_type: contentTypeFromMessage(msg),
			message: msg.body,
			message_id: msg.id._serialized,
			is_reply: !!msg.hasQuotedMsg,
		});
	});

	client.on("message_ack", async (msg, ack) => {
		// ack: -1 error, 1 sent to server, 2 delivered, 3 read
		const statusMap = { "-1": "Failed", 1: "Sent", 2: "Delivered", 3: "Read" };
		const status = statusMap[String(ack)];
		const messageId = msg?.id?._serialized;
		if (status && messageId) {
			await frappeClient.messageStatus(messageId, status);
		}
	});

	return client;
}

async function markConnected(sessionId) {
	const entry = sessions.get(sessionId);
	if (!entry || entry.status === "connected") return;
	entry.status = "connected";
	await frappeClient.updateQr(sessionId, null, "Active");
}

// wwebjs's 'ready' event doesn't always fire even once the page has actually
// finished loading and authenticating — poll the real socket state as a fallback.
function pollStateFallback(sessionId) {
	const interval = setInterval(async () => {
		const entry = sessions.get(sessionId);
		if (!entry) return clearInterval(interval);
		if (entry.status === "connected" || entry.status === "disconnected") {
			return clearInterval(interval);
		}
		try {
			const state = await entry.client.getState();
			if (state !== "CONNECTED") return;

			// WhatsApp Web reloads the page right after a device links, which tears
			// down window.WWebJS (wwebjs's injected bridge) until the reload settles.
			// Calling attachEventListeners() before it's back throws "Cannot read
			// properties of undefined (reading 'injectToFunction')" and can collide
			// with wwebjs's own in-flight injection, destroying its execution context.
			// Confirm the bridge is actually present first — cheap, side-effect-free.
			const bridgeReady = await entry.client.pupPage
				.evaluate(() => typeof window.WWebJS !== "undefined")
				.catch(() => false);
			if (!bridgeReady) return;

			// wwebjs's internal 'ready' flow (which wires up message/ack listeners)
			// can hang indefinitely even once the socket is actually connected and
			// the bridge is present — attach listeners ourselves so inbound messages
			// still fire. Only try once per tick (5s apart); keep polling until it lands.
			try {
				await entry.client.attachEventListeners();
			} catch (err) {
				console.error(`[relay] attachEventListeners failed for ${sessionId}, will retry:`, err.message);
				return;
			}
			await markConnected(sessionId);
			clearInterval(interval);
		} catch {
			// page not ready yet, keep polling
		}
	}, 5000);
}

function startSession(sessionId) {
	if (sessions.has(sessionId)) {
		return sessions.get(sessionId);
	}
	const entry = { client: null, status: "initializing" };
	sessions.set(sessionId, entry);
	entry.client = buildClient(sessionId);
	entry.client.initialize();
	pollStateFallback(sessionId);
	return entry;
}

function getStatus(sessionId) {
	const entry = sessions.get(sessionId);
	return entry ? entry.status : "not_started";
}

async function pauseSession(sessionId) {
	const entry = sessions.get(sessionId);
	if (!entry) return;
	await entry.client.destroy();
	sessions.delete(sessionId);
}

function resumeSession(sessionId) {
	return startSession(sessionId);
}

async function sendMessage(sessionId, to, message, contentType, attachUrl) {
	const entry = sessions.get(sessionId);
	if (!entry || entry.status !== "connected") {
		throw new Error(`Session ${sessionId} is not connected`);
	}
	const chatId = to.includes("@c.us") ? to : `${to}@c.us`;

	if (contentType && contentType !== "text" && attachUrl) {
		const media = await MessageMedia.fromUrl(attachUrl, { unsafeMime: true });
		const sent = await entry.client.sendMessage(chatId, media, { caption: message || undefined });
		// wwebjs can resolve without a Message object even though the send itself succeeded
		return sent?.id?._serialized ?? null;
	}

	const sent = await entry.client.sendMessage(chatId, message);
	return sent?.id?._serialized ?? null;
}

module.exports = { startSession, getStatus, pauseSession, resumeSession, sendMessage };
