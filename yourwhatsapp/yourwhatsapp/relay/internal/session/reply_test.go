package session

import (
	"strings"
	"sync"
	"testing"
	"time"

	"yourwhatsapp/relay/internal/outbox"
	"yourwhatsapp/relay/internal/store"
)

const (
	ownNonAD = "919876543210@s.whatsapp.net"
	peerLID  = "123456789012345@lid"
)

// connectedWithPeer is a connected session "A" that knows peerJID's number.
func connectedWithPeer(h *harness) *fakeClient {
	fc := h.connected("A", testJID)
	fc.registered["+919000000001"] = mustJID(peerJID)
	return fc
}

func (f *fakeClient) lastSent() fakeSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[len(f.sent)-1]
}

func (f *fakeClient) onlyReaction(t *testing.T) fakeReaction {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reactions) != 1 {
		t.Fatalf("reactions = %v, want one", f.reactions)
	}
	return f.reactions[0]
}

func TestReplyQuotesCustomerMessage(t *testing.T) {
	h := newHarness(t)
	fc := connectedWithPeer(h)
	req := SendRequest{SessionID: "A", To: "+919000000001", Message: "answer", LocalID: "m1",
		ReplyToID: "TARGET1", ReplyText: "question"}
	if serr := h.send(req); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("send", func() bool { return fc.sentCount() == 1 })
	ctx := fc.lastSent().msg.GetExtendedTextMessage().GetContextInfo()
	if ctx.GetStanzaID() != "TARGET1" || ctx.GetParticipant() != peerJID || ctx.GetQuotedMessage().GetConversation() != "question" {
		t.Fatalf("context = %v", ctx)
	}
}

func TestReplyQuotesOwnMessage(t *testing.T) {
	h := newHarness(t)
	fc := connectedWithPeer(h)
	req := SendRequest{SessionID: "A", To: "+919000000001", Message: "answer", LocalID: "m1",
		ReplyToID: "TARGET1", ReplyFromMe: true, ReplyText: "we said"}
	if serr := h.send(req); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("send", func() bool { return fc.sentCount() == 1 })
	if p := fc.lastSent().msg.GetExtendedTextMessage().GetContextInfo().GetParticipant(); p != ownNonAD {
		t.Fatalf("participant = %q, want %q", p, ownNonAD)
	}
}

func TestReplyMediaCarriesContext(t *testing.T) {
	h := newHarness(t)
	fc := connectedWithPeer(h)
	req := SendRequest{SessionID: "A", To: "+919000000001", ContentType: "image", Message: "look", LocalID: "m1",
		AttachBase64: "iVBORw0KGgo=", ReplyToID: "TARGET1", ReplyText: "[image]"}
	if serr := h.send(req); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("send", func() bool { return fc.sentCount() == 1 })
	if id := fc.lastSent().msg.GetImageMessage().GetContextInfo().GetStanzaID(); id != "TARGET1" {
		t.Fatalf("image context stanza id = %q", id)
	}
}

func TestReplyMissingTargetSendsPlain(t *testing.T) {
	h := newHarness(t)
	fc := connectedWithPeer(h)
	req := SendRequest{SessionID: "A", To: "+919000000001", Message: "hello", LocalID: "m1", ReplyText: "orphan quote"}
	if serr := h.send(req); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("send", func() bool { return fc.sentCount() == 1 })
	if m := fc.lastSent().msg; m.GetConversation() != "hello" || m.ExtendedTextMessage != nil {
		t.Fatalf("sent %v, want a plain text message", m)
	}
}

func sendReaction(t *testing.T, h *harness, req SendRequest) {
	t.Helper()
	req.SessionID, req.ContentType = "A", "reaction"
	if req.LocalID == "" {
		req.LocalID = "r1"
	}
	if serr := h.send(req); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("reaction sent", func() bool { return len(h.callbacks(outbox.MethodStatus)) == 1 })
}

func TestReactionSenderFromMe(t *testing.T) {
	h := newHarness(t)
	fc := connectedWithPeer(h)
	sendReaction(t, h, SendRequest{To: "+919000000001", Message: "👍", ReplyToID: "OUT1", ReplyFromMe: true})
	r := fc.onlyReaction(t)
	if r.chat.String() != peerJID || r.sender.String() != ownNonAD || r.id != "OUT1" || r.emoji != "👍" {
		t.Fatalf("reaction = %+v", r)
	}
	if fc.lastSent().msg.GetReactionMessage() == nil || fc.lastSent().to.String() != peerJID {
		t.Fatalf("sent %v to %s", fc.lastSent().msg, fc.lastSent().to)
	}
	cb := h.callbacks(outbox.MethodStatus)[0]
	if cb["local_message_id"] != "r1" || cb["status"] != "Sent" {
		t.Fatalf("callback = %v", cb)
	}
	if rec := h.sendRow("r1"); rec.State != store.SendSent {
		t.Fatalf("row = %+v", rec)
	}
}

func TestReactionSenderCustomer(t *testing.T) {
	h := newHarness(t)
	fc := connectedWithPeer(h)
	sendReaction(t, h, SendRequest{To: "+919000000001", Message: "❤️", ReplyToID: "IN1"})
	r := fc.onlyReaction(t)
	if r.chat.String() != peerJID || r.sender.String() != peerJID || r.id != "IN1" {
		t.Fatalf("reaction = %+v", r)
	}
}

func TestReactionSenderLID(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	sendReaction(t, h, SendRequest{To: "123456789012345", ToIsLID: true, Message: "🙏", ReplyToID: "IN1"})
	r := fc.onlyReaction(t)
	if r.chat.String() != peerLID || r.sender.String() != peerLID {
		t.Fatalf("reaction = %+v", r)
	}
}

func TestReactionEmptyEmojiRemoves(t *testing.T) {
	h := newHarness(t)
	fc := connectedWithPeer(h)
	sendReaction(t, h, SendRequest{To: "+919000000001", Message: "", ReplyToID: "IN1"})
	if r := fc.onlyReaction(t); r.emoji != "" || r.id != "IN1" {
		t.Fatalf("reaction = %+v", r)
	}
}

func TestReactionNeedsTarget(t *testing.T) {
	h := newHarness(t)
	connectedWithPeer(h)
	serr := h.send(SendRequest{SessionID: "A", To: "+919000000001", ContentType: "reaction", Message: "👍", LocalID: "r1"})
	if serr == nil || serr.Code != CodeBadRequest {
		t.Fatalf("got %v, want bad_request", serr)
	}
}

// A reaction skips the typing presence, the typing pause and the anti-ban
// gap after the previous send.
func TestReactionSkipsDelay(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var slept []time.Duration
	h.m.sleep = func(d time.Duration) { mu.Lock(); slept = append(slept, d); mu.Unlock() }
	h.m.cfg.SendDelayMin, h.m.cfg.SendDelayMax = 4*time.Second, 4*time.Second
	fc := connectedWithPeer(h)

	if serr := h.send(SendRequest{SessionID: "A", To: "+919000000001", Message: "hi", LocalID: "m1"}); serr != nil {
		t.Fatal(serr)
	}
	if serr := h.send(SendRequest{SessionID: "A", To: "+919000000001", ContentType: "reaction",
		Message: "👍", LocalID: "r1", ReplyToID: "IN1"}); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("both sent", func() bool { return len(h.callbacks(outbox.MethodStatus)) == 2 })
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 1 || slept[0] != time.Second {
		t.Fatalf("sleeps = %v, want only the text's 1s typing pause", slept)
	}
	if n := fc.presenceCount(); n != 2 {
		t.Fatalf("presences = %d, want 2 (text only)", n)
	}
}

// The quote names the number that actually sent the quoted message when
// Frappe knows it (rotation), even though another number replies.
func TestReplyFromMeUsesReplyParticipant(t *testing.T) {
	h := newHarness(t)
	fc := connectedWithPeer(h)
	req := SendRequest{SessionID: "A", To: "+919000000001", Message: "answer", LocalID: "m1",
		ReplyToID: "OUT1", ReplyFromMe: true, ReplyText: "we said", ReplyParticipant: "+91 91111-11111"}
	if serr := h.send(req); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("send", func() bool { return fc.sentCount() == 1 })
	if p := fc.lastSent().msg.GetExtendedTextMessage().GetContextInfo().GetParticipant(); p != "919111111111@s.whatsapp.net" {
		t.Fatalf("participant = %q", p)
	}
}

// A reaction to one of our messages always names this session: Frappe pins
// reactions to the target's account.
func TestReactionFromMeIgnoresReplyParticipant(t *testing.T) {
	h := newHarness(t)
	fc := connectedWithPeer(h)
	sendReaction(t, h, SendRequest{To: "+919000000001", Message: "👍", ReplyToID: "OUT1", ReplyFromMe: true,
		ReplyParticipant: "919111111111"})
	if r := fc.onlyReaction(t); r.sender.String() != ownNonAD {
		t.Fatalf("sender = %s, want %s", r.sender, ownNonAD)
	}
}

// In a LID chat our own messages are authored by our LID, when known.
func TestReplyFromMeInLIDChatUsesOwnLID(t *testing.T) {
	h := newHarness(t)
	h.setup = func(fc *fakeClient) { fc.ownLID = mustJID("555000111222333:5@lid") }
	fc := h.connected("A", testJID)
	req := SendRequest{SessionID: "A", To: "123456789012345", ToIsLID: true, Message: "answer", LocalID: "m1",
		ReplyToID: "OUT1", ReplyFromMe: true, ReplyText: "we said"}
	if serr := h.send(req); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("send", func() bool { return fc.sentCount() == 1 })
	if p := fc.lastSent().msg.GetExtendedTextMessage().GetContextInfo().GetParticipant(); p != "555000111222333@lid" {
		t.Fatalf("participant = %q", p)
	}
}

func TestReplyFromMeInLIDChatWithoutOwnLIDUsesPhone(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	req := SendRequest{SessionID: "A", To: "123456789012345", ToIsLID: true, Message: "answer", LocalID: "m1",
		ReplyToID: "OUT1", ReplyFromMe: true, ReplyText: "we said"}
	if serr := h.send(req); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("send", func() bool { return fc.sentCount() == 1 })
	if p := fc.lastSent().msg.GetExtendedTextMessage().GetContextInfo().GetParticipant(); p != ownNonAD {
		t.Fatalf("participant = %q", p)
	}
}

func TestReactionValidation(t *testing.T) {
	h := newHarness(t)
	connectedWithPeer(h)
	for name, req := range map[string]SendRequest{
		"emoji too long": {Message: strings.Repeat("👍", 17)},
		"attachment":     {Message: "👍", AttachBase64: "iVBORw0KGgo="},
	} {
		req.SessionID, req.To, req.ContentType, req.LocalID, req.ReplyToID = "A", "+919000000001", "reaction", "r-"+name, "IN1"
		if serr := h.send(req); serr == nil || serr.Code != CodeBadRequest {
			t.Errorf("%s: got %v, want bad_request", name, serr)
		}
	}
	ok := SendRequest{SessionID: "A", To: "+919000000001", ContentType: "reaction", LocalID: "r-ok", ReplyToID: "IN1",
		Message: strings.Repeat("👍", 16)}
	if serr := h.send(ok); serr != nil {
		t.Fatalf("16-rune emoji rejected: %v", serr)
	}
}

// A reaction still counts as a send: a text right behind it waits the gap.
func TestTextAfterReactionWaitsGap(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var slept []time.Duration
	h.m.sleep = func(d time.Duration) { mu.Lock(); slept = append(slept, d); mu.Unlock() }
	h.m.cfg.SendDelayMin, h.m.cfg.SendDelayMax = 4*time.Second, 4*time.Second
	connectedWithPeer(h)

	if serr := h.send(SendRequest{SessionID: "A", To: "+919000000001", ContentType: "reaction",
		Message: "👍", LocalID: "r1", ReplyToID: "IN1"}); serr != nil {
		t.Fatal(serr)
	}
	if serr := h.send(SendRequest{SessionID: "A", To: "+919000000001", Message: "hi", LocalID: "m1"}); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("both sent", func() bool { return len(h.callbacks(outbox.MethodStatus)) == 2 })
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 2 || slept[0] <= 3*time.Second || slept[0] > 4*time.Second || slept[1] != time.Second {
		t.Fatalf("sleeps = %v, want [~4s gap, 1s typing]", slept)
	}
}
