package session

import (
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"yourwhatsapp/relay/internal/outbox"
	"yourwhatsapp/relay/internal/store"
)

func TestQueueSuccessMarksSentAndCallback(t *testing.T) {
	h := newHarness(t)
	var typing []time.Duration
	var mu sync.Mutex
	h.m.sleep = func(d time.Duration) { mu.Lock(); typing = append(typing, d); mu.Unlock() }
	fc := h.connected("A", testJID)
	fc.registered["+919000000001"] = mustJID(peerJID)

	if serr := h.send(SendRequest{SessionID: "A", To: "+91 90000-00001", Message: "hello", LocalID: "m1"}); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("status callback", func() bool { return len(h.callbacks(outbox.MethodStatus)) == 1 })

	cb := h.callbacks(outbox.MethodStatus)[0]
	if cb["local_message_id"] != "m1" || cb["message_id"] != "WAID-1" || cb["status"] != "Sent" {
		t.Errorf("callback = %v", cb)
	}
	if rec := h.sendRow("m1"); rec.State != store.SendSent || rec.MessageID != "WAID-1" {
		t.Errorf("row = %+v", rec)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.sent[0].msg.GetConversation() != "hello" || fc.sent[0].to.String() != peerJID {
		t.Errorf("sent %v to %s", fc.sent[0].msg, fc.sent[0].to)
	}
	if len(fc.presences) != 2 || fc.presences[0] != types.ChatPresenceComposing || fc.presences[1] != types.ChatPresencePaused {
		t.Errorf("presences = %v", fc.presences)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(typing) != 1 || typing[0] != time.Second {
		t.Errorf("typing sleeps = %v, want [1s]", typing)
	}
}

func TestQueueSendsMediaWithUpload(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.registered["+919000000001"] = mustJID(peerJID)
	req := SendRequest{SessionID: "A", To: "+919000000001", ContentType: "document", Message: "invoice", LocalID: "m1",
		AttachBase64: "JVBERi0=", AttachMimetype: "application/pdf", AttachFilename: "inv.pdf"}
	if serr := h.send(req); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("send", func() bool { return fc.sentCount() == 1 })
	fc.mu.Lock()
	defer fc.mu.Unlock()
	doc := fc.sent[0].msg.GetDocumentMessage()
	if len(fc.uploads) != 1 || doc.GetFileName() != "inv.pdf" || doc.GetCaption() != "invoice" || doc.GetMimetype() != "application/pdf" {
		t.Errorf("uploads = %v, document = %v", fc.uploads, doc)
	}
}

func TestQueueErrorMarksFailedAndCallback(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.registered["+919000000001"] = mustJID(peerJID)
	fc.sendErr = errFake

	if serr := h.send(SendRequest{SessionID: "A", To: "+919000000001", Message: "hello", LocalID: "m1"}); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("status callback", func() bool { return len(h.callbacks(outbox.MethodStatus)) == 1 })

	cb := h.callbacks(outbox.MethodStatus)[0]
	if cb["local_message_id"] != "m1" || cb["status"] != "Failed" || cb["error_code"] != "send_failed" || cb["error_reason"] == "" {
		t.Errorf("callback = %v", cb)
	}
	if rec := h.sendRow("m1"); rec.State != store.SendFailed || rec.ErrorCode != store.ErrCodeSendFailed {
		t.Errorf("row = %+v", rec)
	}
}

func TestQueueFailsJobWhenSessionDropped(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.m.sleep = func(time.Duration) { <-release }
	fc := h.connected("A", testJID)
	fc.registered["+919000000001"] = mustJID(peerJID)
	for _, id := range []string{"m1", "m2"} {
		if serr := h.send(SendRequest{SessionID: "A", To: "+919000000001", Message: "hi", LocalID: id}); serr != nil {
			t.Fatal(serr)
		}
	}
	h.waitFor("m1 in flight", func() bool { return fc.presenceCount() > 0 })
	fc.Disconnect() // the socket drops while m2 waits
	close(release)
	h.waitFor("m2 failed", func() bool { r := h.sendRow("m2"); return r.State == store.SendFailed })
	if rec := h.sendRow("m2"); rec.ErrorCode != CodeSessionUnavailable {
		t.Errorf("m2 = %+v", rec)
	}
}

func TestQueueDelayWithinBounds(t *testing.T) {
	for _, tc := range []struct {
		rnd  float64
		want time.Duration
	}{{0, 4 * time.Second}, {0.5, 8 * time.Second}, {1, 12 * time.Second}} {
		q := newSendQueue(50, 4*time.Second, 12*time.Second, func(time.Duration) {}, func() float64 { return tc.rnd })
		if got := q.nextDelay(); got != tc.want {
			t.Errorf("rnd %v: delay = %v, want %v", tc.rnd, got, tc.want)
		}
	}
	if got := newSendQueue(50, 0, 0, nil, nil).nextDelay(); got != 0 {
		t.Errorf("0/0 delay = %v", got)
	}

	// The worker waits the delay between sends, but not before the first.
	var mu sync.Mutex
	var slept []time.Duration
	q := newSendQueue(50, 4*time.Second, 12*time.Second,
		func(d time.Duration) { mu.Lock(); slept = append(slept, d); mu.Unlock() },
		func() float64 { return 1 })
	done := make(chan struct{}, 2)
	q.start(func(*sendJob) { done <- struct{}{} })
	defer q.shutdown(time.Second)
	q.push(&sendJob{localID: "a"})
	q.push(&sendJob{localID: "b"})
	<-done
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 1 || slept[0] <= 11*time.Second || slept[0] > 12*time.Second {
		t.Errorf("slept = %v, want one wait just under 12s", slept)
	}
}
