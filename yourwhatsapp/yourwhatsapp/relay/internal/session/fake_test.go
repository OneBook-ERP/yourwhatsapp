package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"

	"yourwhatsapp/relay/internal/config"
	"yourwhatsapp/relay/internal/store"
)

type fakeSent struct {
	to  types.JID
	msg *waE2E.Message
}

// fakeClient is an in-memory WAClient. Connect emits the first QR code when a
// QR channel is open, like whatsmeow does once the websocket is up.
type fakeClient struct {
	mu          sync.Mutex
	own         types.JID
	connected   bool
	handlers    []whatsmeow.EventHandler
	qr          chan whatsmeow.QRChannelItem
	connects    int
	disconnects int
	logouts     int
	proxy       string
	registered  map[string]types.JID
	isOnCalls   []string
	sendErr     error
	sent        []fakeSent
	presences   []types.ChatPresence
	uploads     []whatsmeow.MediaType
	download    []byte
	downloadErr error
	pairPhone   string
}

func newFakeClient(dev *wastore.Device) *fakeClient {
	f := &fakeClient{registered: map[string]types.JID{}}
	if dev.ID != nil {
		f.own = *dev.ID
	}
	return f
}

func (f *fakeClient) Connect() error {
	f.mu.Lock()
	f.connects++
	f.connected = true
	qr := f.qr
	f.mu.Unlock()
	if qr != nil {
		qr <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "2@fake-qr"}
	}
	return nil
}

func (f *fakeClient) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnects++
	f.connected = false
}

func (f *fakeClient) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeClient) IsLoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected && !f.own.IsEmpty()
}

func (f *fakeClient) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts++
	f.connected = false
	return nil
}

func (f *fakeClient) GetQRChannel(context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.qr = make(chan whatsmeow.QRChannelItem, 8)
	return f.qr, nil
}

func (f *fakeClient) PairPhone(_ context.Context, phone string, _ bool, _ whatsmeow.PairClientType, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pairPhone = phone
	return "ABCD-1234", nil
}

func (f *fakeClient) IsOnWhatsApp(_ context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []types.IsOnWhatsAppResponse
	for _, p := range phones {
		f.isOnCalls = append(f.isOnCalls, p)
		jid, ok := f.registered[p]
		out = append(out, types.IsOnWhatsAppResponse{Query: p, JID: jid, IsIn: ok})
	}
	return out, nil
}

func (f *fakeClient) Upload(_ context.Context, _ []byte, mt whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, mt)
	return whatsmeow.UploadResponse{URL: "https://mmg.example/x", DirectPath: "/x"}, nil
}

func (f *fakeClient) SendMessage(_ context.Context, to types.JID, msg *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return whatsmeow.SendResponse{}, f.sendErr
	}
	f.sent = append(f.sent, fakeSent{to, msg})
	return whatsmeow.SendResponse{ID: fmt.Sprintf("WAID-%d", len(f.sent))}, nil
}

func (f *fakeClient) SendChatPresence(_ context.Context, _ types.JID, state types.ChatPresence, _ types.ChatPresenceMedia) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presences = append(f.presences, state)
	return nil
}

func (f *fakeClient) Download(context.Context, whatsmeow.DownloadableMessage) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.download, f.downloadErr
}

func (f *fakeClient) SetProxyAddress(addr string, _ ...whatsmeow.SetProxyOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proxy = addr
	return nil
}

func (f *fakeClient) AddEventHandler(h whatsmeow.EventHandler) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, h)
	return uint32(len(f.handlers))
}

func (f *fakeClient) OwnJID() types.JID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.own
}

// emit delivers an event to every handler synchronously.
func (f *fakeClient) emit(evt any) {
	f.mu.Lock()
	hs := append([]whatsmeow.EventHandler(nil), f.handlers...)
	f.mu.Unlock()
	for _, h := range hs {
		h(evt)
	}
}

func (f *fakeClient) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeClient) presenceCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.presences)
}

type callback struct {
	Method  string
	Payload map[string]any
}

type harness struct {
	t     *testing.T
	m     *Manager
	st    *store.Store
	setup func(*fakeClient)

	mu      sync.Mutex
	calls   []callback
	clients []*fakeClient
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, st: st}
	enqueue := func(_ context.Context, method string, payload any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		var p map[string]any
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.calls = append(h.calls, callback{method, p})
		return nil
	}
	factory := func(dev *wastore.Device) WAClient {
		fc := newFakeClient(dev)
		if h.setup != nil {
			h.setup(fc)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.clients = append(h.clients, fc)
		return fc
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.m = NewManager(st, enqueue, factory, config.Config{}, log)
	h.m.sleep = func(time.Duration) {}
	t.Cleanup(func() {
		h.m.Shutdown(context.Background())
		st.Close()
	})
	return h
}

func (h *harness) callbacks(method string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, c := range h.calls {
		if c.Method == method {
			out = append(out, c.Payload)
		}
	}
	return out
}

func (h *harness) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

func (h *harness) client(i int) *fakeClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clients[i]
}

// waitFor polls cond for up to two seconds.
func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// pairedAccount creates an account row and a saved whatsmeow device for jid.
func (h *harness) pairedAccount(sessionID, jid string) {
	h.t.Helper()
	ctx := context.Background()
	id := mustJID(jid)
	dev := h.st.Container.NewDevice()
	dev.ID = &id
	dev.Account = &waAdv.ADVSignedDeviceIdentity{
		Details: []byte{}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64),
	}
	if err := h.st.Container.PutDevice(ctx, dev); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.UpsertAccount(ctx, sessionID); err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetAccountJID(ctx, sessionID, jid); err != nil {
		h.t.Fatal(err)
	}
}

// connected starts a paired session and reports it connected; returns its client.
func (h *harness) connected(sessionID, jid string) *fakeClient {
	h.t.Helper()
	h.pairedAccount(sessionID, jid)
	if _, err := h.m.Start(context.Background(), sessionID, nil); err != nil {
		h.t.Fatal(err)
	}
	fc := h.client(h.clientCount() - 1)
	fc.emit(eventConnected())
	if st, _, _ := h.m.Status(sessionID); st != StatusConnected {
		h.t.Fatalf("status = %s, want connected", st)
	}
	return fc
}

func (h *harness) send(req SendRequest) *SendError {
	return h.m.Send(context.Background(), req)
}

func (h *harness) sendRow(localID string) *store.SendRecord {
	h.t.Helper()
	rec, err := h.st.GetSend(context.Background(), localID)
	if err != nil {
		h.t.Fatal(err)
	}
	return rec
}

var errFake = errors.New("fake failure")

func mustJID(s string) types.JID {
	jid, err := types.ParseJID(s)
	if err != nil {
		panic(err)
	}
	return jid
}
