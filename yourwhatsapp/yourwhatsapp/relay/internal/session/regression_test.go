package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"yourwhatsapp/relay/internal/outbox"
	"yourwhatsapp/relay/internal/store"
)

func TestReceiptDuringSendOrderedAfterSent(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.registered["+919000000001"] = mustJID(peerJID)
	fc.afterSend = func(id string) {
		fc.emit(&events.Receipt{
			MessageSource: types.MessageSource{Chat: mustJID(peerJID)},
			MessageIDs:    []types.MessageID{id},
			Type:          types.ReceiptTypeDelivered,
		})
	}
	if serr := h.send(SendRequest{SessionID: "A", To: "+919000000001", Message: "hi", LocalID: "m1"}); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("two status callbacks", func() bool { return len(h.callbacks(outbox.MethodStatus)) == 2 })
	cbs := h.callbacks(outbox.MethodStatus)
	if cbs[0]["status"] != "Sent" || cbs[1]["status"] != "Delivered" || cbs[1]["message_id"] != "WAID-1" {
		t.Fatalf("callbacks out of order: %v", cbs)
	}
}

func TestConnectFailureMarksDisconnected(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.emit(&events.ConnectFailure{Reason: events.ConnectFailureReason(499), Message: "unknown"})
	if st, _, _ := h.m.Status("A"); st != StatusDisconnected {
		t.Errorf("status = %s", st)
	}
	if got := statusPushes(h); len(got) != 2 || got[1] != "Disconnected" {
		t.Errorf("status pushes = %v", got)
	}
}

func TestClientOutdatedMarksDisconnected(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.emit(&events.ClientOutdated{})
	if st, _, _ := h.m.Status("A"); st != StatusDisconnected {
		t.Errorf("status = %s", st)
	}
	if got := lastStatusPush(h); got != "Disconnected" {
		t.Errorf("last status push = %q", got)
	}
}

func TestStartRebuildsAfterConnectFailure(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.emit(&events.ConnectFailure{Reason: events.ConnectFailureReason(499)})
	status, err := h.m.Start(context.Background(), "A", nil)
	if err != nil || status != StatusConnecting {
		t.Fatalf("Start = %s, %v", status, err)
	}
	if h.clientCount() != 2 || h.client(1).connects != 1 {
		t.Fatalf("clients = %d, want a rebuilt one", h.clientCount())
	}
	if fc.IsConnected() {
		t.Error("old client left connected")
	}
}

func TestStartDuringShutdownDoesNotLeak(t *testing.T) {
	h := newHarness(t)
	h.setup = func(*fakeClient) { // Shutdown lands while the client is being built
		if err := h.m.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}
	if _, err := h.m.Start(context.Background(), "A", nil); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Start = %v, want ErrShuttingDown", err)
	}
	fc := h.client(0)
	if fc.connects != 0 || fc.disconnects == 0 {
		t.Errorf("connects = %d, disconnects = %d", fc.connects, fc.disconnects)
	}
	if h.m.session("A") != nil {
		t.Error("session registered after shutdown")
	}
}

func TestRestoreEnqueueFailureKeepsRemainingQueued(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, fire := captureAfterFunc(h)
	for _, id := range []string{"m1", "m2"} {
		if _, err := h.st.ReserveSend(ctx, id, "A"); err != nil {
			t.Fatal(err)
		}
	}
	h.enqueueErr = func(n int) error {
		if n == 2 {
			return errFake
		}
		return nil
	}
	if err := h.m.RestoreAll(ctx); err != nil {
		t.Fatal(err)
	}
	h.m.FailLostSendsAfter(time.Second)
	fire()
	if rec := h.sendRow("m1"); rec.State != store.SendFailed {
		t.Errorf("m1 = %+v, want failed (callback enqueued)", rec)
	}
	if rec := h.sendRow("m2"); rec.State != store.SendQueued {
		t.Errorf("m2 = %+v, want still queued for the next boot", rec)
	}

	// Next boot.
	h.enqueueErr = nil
	if err := h.m.RestoreAll(ctx); err != nil {
		t.Fatal(err)
	}
	h.m.FailLostSendsAfter(time.Second)
	fire()
	cbs := h.callbacks(outbox.MethodStatus)
	if len(cbs) != 2 || cbs[1]["local_message_id"] != "m2" {
		t.Errorf("callbacks = %v", cbs)
	}
}

func TestPauseDuringTypingDoesNotSend(t *testing.T) {
	h := newHarness(t)
	h.m.sleep = nil // real typing sleep (1 s), interrupted by Pause
	fc := h.connected("A", testJID)
	fc.registered["+919000000001"] = mustJID(peerJID)
	if serr := h.send(SendRequest{SessionID: "A", To: "+919000000001", Message: "hi", LocalID: "m1"}); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("typing", func() bool { return fc.presenceCount() > 0 })
	start := time.Now()
	if err := h.m.Pause(context.Background(), "A"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("pause waited out the typing delay")
	}
	if fc.sentCount() != 0 {
		t.Fatal("message sent after pause")
	}
	if rec := h.sendRow("m1"); rec.State != store.SendFailed || rec.ErrorCode != CodeSessionUnavailable {
		t.Errorf("m1 = %+v", rec)
	}
	cbs := h.callbacks(outbox.MethodStatus)
	if len(cbs) != 1 || cbs[0]["status"] != "Failed" || cbs[0]["error_code"] != CodeSessionUnavailable {
		t.Errorf("callbacks = %v", cbs)
	}
}
