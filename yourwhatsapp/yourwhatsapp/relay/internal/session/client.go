// Package session runs one whatsmeow client per WhatsApp account: pairing,
// lifecycle events, inbound messages and the per-account send queue.
package session

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// WAClient is the subset of *whatsmeow.Client the relay uses.
type WAClient interface {
	Connect() error
	Disconnect()
	IsConnected() bool
	IsLoggedIn() bool
	Logout(ctx context.Context) error
	GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	PairPhone(ctx context.Context, phone string, showPushNotification bool, clientType whatsmeow.PairClientType, clientDisplayName string) (string, error)
	IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)
	Upload(ctx context.Context, plaintext []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	SendChatPresence(ctx context.Context, jid types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error
	Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)
	SetProxyAddress(addr string, opts ...whatsmeow.SetProxyOptions) error
	AddEventHandler(handler whatsmeow.EventHandler) uint32
	OwnJID() types.JID
}

// ClientFactory builds a client for a device loaded from (or new to) the store.
type ClientFactory func(dev *wastore.Device) WAClient

// NewWhatsmeowClient is the production ClientFactory. whatsmeow's own logger
// is left as a no-op so nothing it logs can leak message content.
func NewWhatsmeowClient(dev *wastore.Device) WAClient {
	c := whatsmeow.NewClient(dev, nil)
	c.EnableAutoReconnect = true
	return &whatsmeowClient{Client: c}
}

type whatsmeowClient struct {
	*whatsmeow.Client
}

func (c *whatsmeowClient) OwnJID() types.JID {
	if id := c.Store.ID; id != nil {
		return *id
	}
	return types.EmptyJID
}
