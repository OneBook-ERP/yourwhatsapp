package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"yourwhatsapp/relay/internal/outbox"
)

const (
	testJID  = "919876543210:5@s.whatsapp.net"
	peerJID  = "919000000001@s.whatsapp.net"
	peerUser = "919000000001"
)

func eventConnected() *events.Connected { return &events.Connected{} }

func statusPushes(h *harness) []string {
	var out []string
	for _, p := range h.callbacks(outbox.MethodQR) {
		if s, ok := p["status"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func lastStatusPush(h *harness) string {
	pushes := h.callbacks(outbox.MethodQR)
	for i := len(pushes) - 1; i >= 0; i-- {
		if s, ok := pushes[i]["status"].(string); ok {
			return s
		}
	}
	return ""
}

func TestQRCodeEnqueuesUpdateQR(t *testing.T) {
	h := newHarness(t)
	status, err := h.m.Start(context.Background(), "A", nil)
	if err != nil || status != StatusQRPending {
		t.Fatalf("Start = %q, %v", status, err)
	}
	h.waitFor("qr push", func() bool { return len(h.callbacks(outbox.MethodQR)) == 1 })
	p := h.callbacks(outbox.MethodQR)[0]
	if p["session_id"] != "A" {
		t.Errorf("session_id = %v", p["session_id"])
	}
	png, err := base64.StdEncoding.DecodeString(p["qr_base64"].(string))
	if err != nil || !bytes.HasPrefix(png, []byte("\x89PNG")) {
		t.Fatalf("qr_base64 is not a PNG (err %v)", err)
	}

	h.client(0).qr <- whatsmeow.QRChannelTimeout
	h.waitFor("disconnected push", func() bool { return lastStatusPush(h) == "Disconnected" })
	if st, _, _ := h.m.Status("A"); st != StatusDisconnected {
		t.Errorf("status = %s, want disconnected", st)
	}
}

func TestPairSuccessStoresJIDAndActive(t *testing.T) {
	h := newHarness(t)
	if _, err := h.m.Start(context.Background(), "A", nil); err != nil {
		t.Fatal(err)
	}
	h.client(0).emit(&events.PairSuccess{ID: mustJID(testJID)})

	acct, _, err := h.st.GetAccount(context.Background(), "A")
	if err != nil || acct.JID != testJID {
		t.Fatalf("account jid = %q, %v", acct.JID, err)
	}
	if got := lastStatusPush(h); got != "Active" {
		t.Errorf("last status push = %q, want Active", got)
	}
	status, jid, phone := h.m.Status("A")
	if status != StatusConnected || phone != "919876543210" || jid == "" {
		t.Errorf("Status = %s %s %s", status, jid, phone)
	}
	// The reconnect that follows pairing must not push Active a second time.
	h.client(0).emit(eventConnected())
	if got := statusPushes(h); len(got) != 1 {
		t.Errorf("status pushes = %v, want one Active", got)
	}
}

func TestLoggedOutClearsJIDAndDisconnected(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})

	acct, _, _ := h.st.GetAccount(context.Background(), "A")
	if acct.JID != "" {
		t.Errorf("jid = %q, want cleared", acct.JID)
	}
	if st, _, _ := h.m.Status("A"); st != StatusLoggedOut {
		t.Errorf("status = %s", st)
	}
	if got := lastStatusPush(h); got != "Disconnected" {
		t.Errorf("last status push = %q", got)
	}
}

func TestStreamReplacedNoReconnect(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.emit(&events.StreamReplaced{})

	if fc.IsConnected() || fc.disconnects == 0 {
		t.Error("client not disconnected")
	}
	if fc.connects != 1 {
		t.Errorf("connects = %d, want 1 (no reconnect)", fc.connects)
	}
	if st, _, _ := h.m.Status("A"); st != StatusDisconnected {
		t.Errorf("status = %s", st)
	}
	if got := lastStatusPush(h); got != "Disconnected" {
		t.Errorf("last status push = %q", got)
	}
}

func TestTemporaryBanEnqueuesAccountEvent(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	h.m.now = func() time.Time { return now }
	var banWait time.Duration
	var reconnect func()
	h.m.afterFunc = func(d time.Duration, f func()) func() bool {
		banWait, reconnect = d, f
		return func() bool { return true }
	}
	fc := h.connected("A", testJID)
	fc.emit(&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: time.Hour})

	evs := h.callbacks(outbox.MethodAccountEvent)
	if len(evs) != 1 {
		t.Fatalf("account events = %v", evs)
	}
	want := map[string]any{"session_id": "A", "event": "temp_banned", "until": "2026-10-31T13:00:00Z"}
	for k, v := range want {
		if evs[0][k] != v {
			t.Errorf("%s = %v, want %v", k, evs[0][k], v)
		}
	}
	if st, _, _ := h.m.Status("A"); st != StatusTempBanned {
		t.Errorf("status = %s", st)
	}
	serr := h.send(SendRequest{SessionID: "A", To: "+91 90000 00001", Message: "hi", LocalID: "m1"})
	if serr == nil || serr.Code != CodeTempBanned || serr.HTTP != 429 {
		t.Fatalf("send while banned = %+v", serr)
	}

	if banWait != time.Hour || reconnect == nil {
		t.Fatalf("reconnect scheduled after %v", banWait)
	}
	fc.Disconnect()
	reconnect()
	if fc.connects != 2 {
		t.Errorf("connects = %d, want 2 after ban expiry", fc.connects)
	}
	fc.emit(eventConnected())
	if got := lastStatusPush(h); got != "Active" {
		t.Errorf("last status push = %q, want Active", got)
	}
}

func inboundImage(id string) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: mustJID(peerJID), Sender: mustJID(peerJID)},
			ID:            id,
		},
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Mimetype: proto.String("image/jpeg"), Caption: proto.String("look"),
		}},
	}
}

func TestInboundEnqueuesWithMedia(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.download = []byte("jpeg-bytes")
	fc.emit(inboundImage("MSG1"))

	in := h.callbacks(outbox.MethodInbound)
	if len(in) != 1 {
		t.Fatalf("inbound = %v", in)
	}
	p := in[0]
	if p["media_base64"] != base64.StdEncoding.EncodeToString([]byte("jpeg-bytes")) {
		t.Errorf("media_base64 = %v", p["media_base64"])
	}
	if p["from_number"] != peerUser || p["content_type"] != "image" || p["media_filename"] != "MSG1.jpg" {
		t.Errorf("payload = %v", p)
	}
}

func TestInboundDownloadFailureStillDelivers(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.downloadErr = errFake
	fc.emit(inboundImage("MSG2"))

	in := h.callbacks(outbox.MethodInbound)
	if len(in) != 1 {
		t.Fatalf("inbound = %v", in)
	}
	if _, has := in[0]["media_base64"]; has {
		t.Error("media_base64 present after failed download")
	}
	if in[0]["message_id"] != "MSG2" || in[0]["message"] != "look" {
		t.Errorf("payload = %v", in[0])
	}
}

// Media over MEDIA_MAX_BYTES is not downloaded (declared size) or not
// forwarded (undeclared size); the message still arrives with a note.
func TestInboundMediaOverCapDeliversNote(t *testing.T) {
	const capBytes = 10 << 20
	declared := inboundImage("BIG1")
	declared.Message.ImageMessage.FileLength = proto.Uint64(20 << 20)
	undeclared := inboundImage("BIG2")
	doc := inboundImage("BIG3")
	doc.Message = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Mimetype: proto.String("application/pdf"), FileName: proto.String("scan.pdf"), FileLength: proto.Uint64(31457280),
	}}
	cases := []struct {
		name          string
		evt           *events.Message
		download      []byte
		wantDownloads int
		wantMessage   string
	}{
		{"declared size", declared, nil, 0, "look\n[attachment too large: image, 20.0 MB]"},
		{"undeclared size", undeclared, make([]byte, capBytes+1), 1, "look\n[attachment too large: image, 10.0 MB]"},
		{"document without caption", doc, nil, 0, "[attachment too large: scan.pdf, 30.0 MB]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.m.cfg.MediaMaxBytes = capBytes
			fc := h.connected("A", testJID)
			fc.download = c.download
			fc.emit(c.evt)

			in := h.callbacks(outbox.MethodInbound)
			if len(in) != 1 {
				t.Fatalf("inbound = %v", in)
			}
			p := in[0]
			if p["message"] != c.wantMessage || p["content_type"] != "text" {
				t.Errorf("message = %q, content_type = %v", p["message"], p["content_type"])
			}
			for _, k := range []string{"media_base64", "media_mimetype", "media_filename"} {
				if _, has := p[k]; has {
					t.Errorf("%s present", k)
				}
			}
			if fc.downloads != c.wantDownloads {
				t.Errorf("downloads = %d, want %d", fc.downloads, c.wantDownloads)
			}
		})
	}
}

func TestReceiptEnqueuesStatus(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.emit(&events.Receipt{
		MessageSource: types.MessageSource{Chat: mustJID(peerJID)},
		MessageIDs:    []types.MessageID{"W1", "W2"},
		Type:          types.ReceiptTypeRead,
	})
	fc.emit(&events.Receipt{ // group receipts are ignored
		MessageSource: types.MessageSource{Chat: mustJID("12345@g.us")},
		MessageIDs:    []types.MessageID{"G1"},
		Type:          types.ReceiptTypeRead,
	})

	st := h.callbacks(outbox.MethodStatus)
	if len(st) != 2 {
		t.Fatalf("status callbacks = %v", st)
	}
	for i, id := range []string{"W1", "W2"} {
		if st[i]["message_id"] != id || st[i]["status"] != "Read" {
			t.Errorf("callback %d = %v", i, st[i])
		}
	}
}
