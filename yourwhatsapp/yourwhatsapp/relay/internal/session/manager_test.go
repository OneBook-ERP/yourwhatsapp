package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"yourwhatsapp/relay/internal/outbox"
	"yourwhatsapp/relay/internal/store"
)

func TestStartIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, err := h.m.Start(ctx, "A", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.m.Start(ctx, "A", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first != StatusQRPending || second != StatusQRPending {
		t.Errorf("statuses = %s, %s", first, second)
	}
	if h.clientCount() != 1 || h.client(0).connects != 1 {
		t.Errorf("clients = %d, connects = %d; want 1, 1", h.clientCount(), h.client(0).connects)
	}
	if _, found, _ := h.st.GetAccount(ctx, "A"); !found {
		t.Error("account row not created")
	}
}

func TestStartStoresProxy(t *testing.T) {
	h := newHarness(t)
	proxy := "socks5://10.0.0.1:1080"
	if _, err := h.m.Start(context.Background(), "A", &proxy); err != nil {
		t.Fatal(err)
	}
	if h.client(0).proxy != proxy {
		t.Errorf("client proxy = %q", h.client(0).proxy)
	}
	acct, _, _ := h.st.GetAccount(context.Background(), "A")
	if acct.Proxy != proxy {
		t.Errorf("stored proxy = %q", acct.Proxy)
	}
}

func TestStartAfterLogoutRebuilds(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	fc := h.connected("A", testJID)
	if err := h.m.Logout(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	if fc.logouts != 1 {
		t.Errorf("logouts = %d", fc.logouts)
	}
	if st, _, _ := h.m.Status("A"); st != StatusLoggedOut {
		t.Errorf("status after logout = %s", st)
	}
	if acct, _, _ := h.st.GetAccount(ctx, "A"); acct.JID != "" {
		t.Errorf("jid = %q, want cleared", acct.JID)
	}

	status, err := h.m.Start(ctx, "A", nil)
	if err != nil || status != StatusQRPending {
		t.Fatalf("Start after logout = %s, %v", status, err)
	}
	if h.clientCount() != 2 {
		t.Fatalf("clients = %d, want a fresh one", h.clientCount())
	}
	h.waitFor("qr push", func() bool { return len(h.callbacks(outbox.MethodQR)) > 0 })
}

func TestPairCodeReturnsCodeWithoutPushingQR(t *testing.T) {
	h := newHarness(t)
	code, err := h.m.PairCode(context.Background(), "A", "+91 98765 43210", nil)
	if err != nil || code != "ABCD-1234" {
		t.Fatalf("PairCode = %q, %v", code, err)
	}
	fc := h.client(0)
	if fc.pairPhone != "+919876543210" || fc.connects != 1 {
		t.Errorf("pairPhone = %q, connects = %d", fc.pairPhone, fc.connects)
	}
	if st, _, _ := h.m.Status("A"); st != StatusPairing {
		t.Errorf("status = %s", st)
	}
	for _, p := range h.callbacks(outbox.MethodQR) {
		if _, has := p["qr_base64"]; has {
			t.Error("QR code pushed during pair-code flow")
		}
	}
}

func TestPairCodeStoresProxy(t *testing.T) {
	h := newHarness(t)
	proxy := "socks5://10.0.0.1:1080"
	if _, err := h.m.PairCode(context.Background(), "A", "+919876543210", &proxy); err != nil {
		t.Fatal(err)
	}
	acct, _, _ := h.st.GetAccount(context.Background(), "A")
	if acct.Proxy != proxy {
		t.Errorf("stored proxy = %q", acct.Proxy)
	}
}

func TestPairCodeRejectsPairedSession(t *testing.T) {
	h := newHarness(t)
	h.connected("A", testJID)
	if _, err := h.m.PairCode(context.Background(), "A", "+919876543210", nil); !errors.Is(err, ErrAlreadyPaired) {
		t.Fatalf("err = %v, want ErrAlreadyPaired", err)
	}
}

// captureAfterFunc replaces the manager's timer with one the test fires.
func captureAfterFunc(h *harness) (delay *time.Duration, fire func()) {
	var d time.Duration
	var f func()
	h.m.afterFunc = func(wait time.Duration, fn func()) func() bool {
		d, f = wait, fn
		return func() bool { return true }
	}
	return &d, func() { f() }
}

// Sends lost with the previous process are reported Failed only after the
// grace period, so restored sessions are back up when Frappe fails over;
// sends accepted after boot are not touched.
func TestRestoreFailsQueuedAfterGrace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	delay, fire := captureAfterFunc(h)
	if _, err := h.st.ReserveSend(ctx, "m1", "A"); err != nil {
		t.Fatal(err)
	}
	if err := h.m.RestoreAll(ctx); err != nil {
		t.Fatal(err)
	}
	h.m.FailLostSendsAfter(30 * time.Second)
	if *delay != 30*time.Second {
		t.Errorf("grace = %v", *delay)
	}
	if cbs := h.callbacks(outbox.MethodStatus); len(cbs) != 0 {
		t.Fatalf("callbacks before grace = %v", cbs)
	}
	if rec := h.sendRow("m1"); rec.State != store.SendQueued {
		t.Fatalf("row before grace = %+v", rec)
	}
	if _, err := h.st.ReserveSend(ctx, "m2", "A"); err != nil { // accepted after boot
		t.Fatal(err)
	}

	fire()
	if rec := h.sendRow("m1"); rec.State != store.SendFailed || rec.ErrorCode != store.ErrCodeRelayRestarted {
		t.Errorf("m1 = %+v", rec)
	}
	if rec := h.sendRow("m2"); rec.State != store.SendQueued {
		t.Errorf("m2 = %+v, want untouched", rec)
	}
	cbs := h.callbacks(outbox.MethodStatus)
	if len(cbs) != 1 || cbs[0]["local_message_id"] != "m1" || cbs[0]["status"] != "Failed" || cbs[0]["error_code"] != "relay_restarted" {
		t.Errorf("callbacks = %v", cbs)
	}
}

func TestRestoreConnectsRestorable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pairedAccount("A", testJID)
	h.pairedAccount("B", "919811111111:3@s.whatsapp.net")
	if err := h.st.SetAccountPaused(ctx, "B", true); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpsertAccount(ctx, "C"); err != nil { // never paired
		t.Fatal(err)
	}
	if err := h.m.RestoreAll(ctx); err != nil {
		t.Fatal(err)
	}
	if h.clientCount() != 1 || h.client(0).connects != 1 {
		t.Fatalf("clients = %d, want only A connected", h.clientCount())
	}
	if st, _, phone := h.m.Status("A"); st != StatusConnecting || phone != "919876543210" {
		t.Errorf("A = %s %s", st, phone)
	}
	if st, _, _ := h.m.Status("B"); st != StatusPaused {
		t.Errorf("B = %s", st)
	}
	if st, _, _ := h.m.Status("C"); st != StatusNotStarted {
		t.Errorf("C = %s", st)
	}
}

func TestPauseAndResume(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	fc := h.connected("A", testJID)
	if err := h.m.Pause(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	if fc.IsConnected() {
		t.Error("client still connected after pause")
	}
	if st, _, _ := h.m.Status("A"); st != StatusPaused {
		t.Errorf("status = %s", st)
	}
	// Start must not override Frappe's pause.
	if st, _ := h.m.Start(ctx, "A", nil); st != StatusPaused || h.clientCount() != 1 {
		t.Errorf("Start on paused = %s, clients = %d", st, h.clientCount())
	}
	if st, err := h.m.Resume(ctx, "A", nil); err != nil || st != StatusConnecting {
		t.Fatalf("Resume = %s, %v", st, err)
	}
	if h.clientCount() != 2 || h.client(1).connects != 1 {
		t.Errorf("resume did not reconnect: clients = %d", h.clientCount())
	}
}

// Frappe may have missed (or overridden) the Active push; Start and Resume
// on an already-connected session report connected and push Active again.
func TestStartAndResumeOnConnectedRepushActive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.connected("A", testJID)
	if got := statusPushes(h); len(got) != 1 || got[0] != "Active" {
		t.Fatalf("pushes after connect = %v", got)
	}
	if st, err := h.m.Start(ctx, "A", nil); err != nil || st != StatusConnected {
		t.Fatalf("Start = %s, %v", st, err)
	}
	if st, err := h.m.Resume(ctx, "A", nil); err != nil || st != StatusConnected {
		t.Fatalf("Resume = %s, %v", st, err)
	}
	if got := statusPushes(h); len(got) != 3 || got[1] != "Active" || got[2] != "Active" {
		t.Errorf("pushes = %v, want Active re-pushed by Start and Resume", got)
	}
	if h.clientCount() != 1 {
		t.Errorf("clients = %d, want the running one kept", h.clientCount())
	}
}

func TestLogoutClearsPaused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.connected("A", testJID)
	if err := h.m.Pause(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Logout(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	if acct, _, _ := h.st.GetAccount(ctx, "A"); acct.Paused {
		t.Error("account still paused after logout")
	}
	if st, err := h.m.Start(ctx, "A", nil); err != nil || st != StatusQRPending {
		t.Errorf("Start after logout = %s, %v; want a fresh QR flow", st, err)
	}
}

func TestSendSessionUnavailable(t *testing.T) {
	h := newHarness(t)
	req := SendRequest{SessionID: "A", To: "+919000000001", Message: "hi", LocalID: "m1"}
	if serr := h.send(req); serr == nil || serr.Code != CodeSessionUnavailable || serr.HTTP != 409 {
		t.Fatalf("unknown session = %+v", serr)
	}
	if _, err := h.m.Start(context.Background(), "A", nil); err != nil { // qr_pending
		t.Fatal(err)
	}
	if serr := h.send(req); serr == nil || serr.Code != CodeSessionUnavailable {
		t.Fatalf("unpaired session = %+v", serr)
	}
	if h.sendRow("m1") != nil {
		t.Error("send row reserved for a rejected send")
	}
}

func TestSendBadRequest(t *testing.T) {
	h := newHarness(t)
	h.connected("A", testJID)
	for name, req := range map[string]SendRequest{
		"no local id": {SessionID: "A", To: "+919000000001", Message: "hi"},
		"no content":  {SessionID: "A", To: "+919000000001", LocalID: "m1"},
		"bad base64":  {SessionID: "A", To: "+919000000001", LocalID: "m1", AttachBase64: "%%%"},
	} {
		if serr := h.send(req); serr == nil || serr.Code != CodeBadRequest || serr.HTTP != 400 {
			t.Errorf("%s: %+v", name, serr)
		}
	}
}

func TestSendInvalidNumber(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	serr := h.send(SendRequest{SessionID: "A", To: "+91 90000 00001", Message: "hi", LocalID: "m1"})
	if serr == nil || serr.Code != CodeInvalidNumber || serr.HTTP != 422 {
		t.Fatalf("unregistered = %+v", serr)
	}
	serr = h.send(SendRequest{SessionID: "A", To: "123", Message: "hi", LocalID: "m2"})
	if serr == nil || serr.Code != CodeInvalidNumber || serr.HTTP != 422 {
		t.Fatalf("too short = %+v", serr)
	}
	if len(fc.isOnCalls) != 1 || fc.isOnCalls[0] != "+919000000001" {
		t.Errorf("IsOnWhatsApp calls = %v", fc.isOnCalls)
	}
}

func TestSendNumberCheckCached(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.registered["+919000000001"] = mustJID(peerJID)
	for _, id := range []string{"m1", "m2"} {
		if serr := h.send(SendRequest{SessionID: "A", To: "+919000000001", Message: "hi", LocalID: id}); serr != nil {
			t.Fatal(serr)
		}
	}
	h.waitFor("two sends", func() bool { return fc.sentCount() == 2 })
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.isOnCalls) != 1 {
		t.Errorf("IsOnWhatsApp calls = %d, want 1 (cached)", len(fc.isOnCalls))
	}
}

func TestSendToLIDSkipsCheck(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	if serr := h.send(SendRequest{SessionID: "A", To: "123456789012345", ToIsLID: true, Message: "hi", LocalID: "m1"}); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("send", func() bool { return fc.sentCount() == 1 })
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.isOnCalls) != 0 {
		t.Errorf("IsOnWhatsApp called: %v", fc.isOnCalls)
	}
	if got := fc.sent[0].to.String(); got != "123456789012345@lid" {
		t.Errorf("target = %s", got)
	}
}

func TestSendDuplicate(t *testing.T) {
	h := newHarness(t)
	fc := h.connected("A", testJID)
	fc.registered["+919000000001"] = mustJID(peerJID)
	req := SendRequest{SessionID: "A", To: "+919000000001", Message: "hi", LocalID: "m1"}
	if serr := h.send(req); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("sent", func() bool { r := h.sendRow("m1"); return r != nil && r.State == store.SendSent })

	serr := h.send(req)
	if serr == nil || serr.Code != CodeDuplicate || serr.HTTP != 200 || serr.Existing == nil {
		t.Fatalf("duplicate = %+v", serr)
	}
	if serr.Existing.State != store.SendSent || serr.Existing.MessageID != "WAID-1" {
		t.Errorf("existing = %+v", serr.Existing)
	}
	if fc.sentCount() != 1 {
		t.Errorf("sent %d times", fc.sentCount())
	}
}

func TestSendQueueFull(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.m.sleep = func(d time.Duration) { <-release }
	h.m.queueCapacity = 1
	fc := h.connected("A", testJID)
	fc.registered["+919000000001"] = mustJID(peerJID)
	req := func(id string) SendRequest {
		return SendRequest{SessionID: "A", To: "+919000000001", Message: "hi", LocalID: id}
	}

	if serr := h.send(req("m1")); serr != nil {
		t.Fatal(serr)
	}
	h.waitFor("m1 in flight", func() bool { return fc.presenceCount() > 0 })
	if serr := h.send(req("m2")); serr != nil {
		t.Fatal(serr)
	}
	serr := h.send(req("m3"))
	if serr == nil || serr.Code != CodeSessionUnavailable || serr.HTTP != 409 {
		t.Fatalf("full queue = %+v", serr)
	}
	if h.sendRow("m3") != nil {
		t.Error("m3 reserved although the queue was full")
	}
	close(release)
}

func TestShutdownDisconnectsAll(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.connected("A", testJID)
	b := h.connected("B", "919811111111:3@s.whatsapp.net")
	if err := h.m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if a.IsConnected() || b.IsConnected() {
		t.Error("client still connected after shutdown")
	}
	if _, err := h.m.Start(ctx, "C", nil); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("Start after shutdown = %v", err)
	}
}
