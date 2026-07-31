const express = require("express");
const sessionManager = require("./session_manager");

const PORT = process.env.RELAY_PORT || 8800;
const RELAY_API_KEY = process.env.RELAY_API_KEY;

if (!RELAY_API_KEY) {
	console.error("RELAY_API_KEY env var is required — set it to the same value as WhatsApp Settings.relay_api_key");
	process.exit(1);
}

// A crash/disconnect on one session's browser must not take down every other
// paired number sharing this process.
process.on("unhandledRejection", (reason) => {
	console.error("[relay] unhandled rejection:", reason);
});
process.on("uncaughtException", (err) => {
	console.error("[relay] uncaught exception:", err);
});

const app = express();
app.use(express.json());

app.use((req, res, next) => {
	if (req.headers["x-api-key"] !== RELAY_API_KEY) {
		return res.status(401).json({ error: "invalid api key" });
	}
	next();
});

app.post("/session/:id/start", (req, res) => {
	sessionManager.startSession(req.params.id);
	res.json({ status: sessionManager.getStatus(req.params.id) });
});

app.get("/session/:id/status", (req, res) => {
	res.json({ status: sessionManager.getStatus(req.params.id) });
});

app.post("/session/:id/pause", async (req, res) => {
	await sessionManager.pauseSession(req.params.id);
	res.json({ status: "paused" });
});

app.post("/session/:id/resume", (req, res) => {
	sessionManager.resumeSession(req.params.id);
	res.json({ status: sessionManager.getStatus(req.params.id) });
});

app.post("/message/send", async (req, res) => {
	const { session_id, to, content_type, message, attach_url } = req.body;
	if (!session_id || !to) {
		return res.status(400).json({ error: "session_id and to are required" });
	}
	try {
		const message_id = await sessionManager.sendMessage(session_id, to, message, content_type, attach_url);
		res.json({ message_id });
	} catch (err) {
		console.error("[relay] /message/send failed:", err.stack || err.message);
		res.status(502).json({ error: err.message });
	}
});

app.listen(PORT, () => {
	console.log(`yourwhatsapp relay listening on :${PORT}`);
});
