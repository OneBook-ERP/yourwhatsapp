const path = require("path");
const qrcode = require("qrcode");
const { Client, LocalAuth, MessageMedia } = require("whatsapp-web.js");
const frappeClient = require("./frappe_client");

const SESSIONS_DIR = path.join(__dirname, ".sessions");

// session_id -> { client, status }
const sessions = new Map();

// WhatsApp's July 2026 Web update renamed the internal serialized-id getter
// from `_serialized` to `$1` on message/chat id objects. whatsapp-web.js
// 1.34.7 still reads `._serialized` internally (e.g. inside downloadMedia()),
// which is now undefined — breaking media download, ack tracking, and any
// other id-dependent call. Backfill it wherever an id object enters our code
// so both our own reads and the library's internal reads see a real value.
// Remove once upstream ships a fix (tracked: wwebjs/whatsapp-web.js#201833).
function serializedId(id) {
	return id?._serialized ?? id?.$1 ?? null;
}

function patchSerializedId(id) {
	if (id && id._serialized == null && id.$1 != null) {
		id._serialized = id.$1;
	}
	return id;
}

// Only 1:1 chats map onto a single CRM contact. Status updates (status@broadcast),
// groups (@g.us), broadcast lists and channels (@newsletter) have no one
// counterpart number, so they'd only land in CRM as unlinkable noise.
function isDirectChat(waId) {
	return typeof waId === "string" && (waId.endsWith("@c.us") || waId.endsWith("@lid"));
}

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
		if (msg.fromMe || !isDirectChat(msg.from)) return;
		patchSerializedId(msg.id);

		let media = null;
		if (msg.hasMedia) {
			try {
				media = await msg.downloadMedia();
				if (!media) {
					console.error(`[relay] downloadMedia returned empty for message ${serializedId(msg.id)}, type=${msg.type}`);
				}
			} catch (err) {
				console.error(`[relay] downloadMedia failed for message ${serializedId(msg.id)}, type=${msg.type}:`, err.message);
			}
		}

		await frappeClient.inboundMessage({
			session_id: sessionId,
			from_number: await resolvePhoneNumber(client, msg.from),
			content_type: contentTypeFromMessage(msg),
			message: msg.body,
			message_id: serializedId(msg.id),
			is_reply: !!msg.hasQuotedMsg,
			media_base64: media?.data ?? null,
			media_mimetype: media?.mimetype ?? null,
			media_filename: media?.filename ?? null,
		});
	});

	client.on("message_ack", async (msg, ack) => {
		// ack: -1 error, 1 sent to server, 2 delivered, 3 read
		if (!isDirectChat(msg?.fromMe ? msg.to : msg?.from)) return;
		const statusMap = { "-1": "Failed", 1: "Sent", 2: "Delivered", 3: "Read" };
		const status = statusMap[String(ack)];
		const messageId = serializedId(msg?.id);
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

async function sendMessage(sessionId, to, message, contentType, attach) {
	const entry = sessions.get(sessionId);
	if (!entry || entry.status !== "connected") {
		throw new Error(`Session ${sessionId} is not connected`);
	}
	const chatId = to.includes("@c.us") ? to : `${to}@c.us`;

	if (contentType && contentType !== "text" && attach?.base64) {
		// Built from raw bytes Frappe already read and encoded — no fetch by
		// the relay itself, so this works regardless of the file's privacy
		// setting or whether its URL is reachable from this process.
		const media = new MessageMedia(attach.mimetype, attach.base64, attach.filename);
		const sent = await entry.client.sendMessage(chatId, media, { caption: message || undefined });
		// wwebjs can resolve without a Message object even though the send itself succeeded
		return serializedId(sent?.id);
	}

	const sent = await entry.client.sendMessage(chatId, message);
	return serializedId(sent?.id);
}

module.exports = { startSession, getStatus, pauseSession, resumeSession, sendMessage };
