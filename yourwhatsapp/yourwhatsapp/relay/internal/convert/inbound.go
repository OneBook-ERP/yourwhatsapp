package convert

import (
	"mime"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// InboundPayload is the body of Frappe's api.inbound_message. MediaBase64 is
// filled by the session layer after the media is downloaded.
type InboundPayload struct {
	SessionID        string `json:"session_id"`
	FromNumber       string `json:"from_number"`
	ContentType      string `json:"content_type"`
	Message          string `json:"message"`
	MessageID        string `json:"message_id"`
	IsReply          bool   `json:"is_reply"`
	ReplyToMessageID string `json:"reply_to_message_id"`
	FromLID          bool   `json:"from_lid"`
	MediaBase64      string `json:"media_base64,omitempty"`
	MediaMimetype    string `json:"media_mimetype,omitempty"`
	MediaFilename    string `json:"media_filename,omitempty"`
}

// IsDirectChat reports whether the JID is a one-to-one chat address.
func IsDirectChat(jid types.JID) bool {
	return jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer
}

// Inbound converts an incoming message into a Frappe payload. media is
// non-nil when the message carries an attachment to download. ok is false for
// messages the relay does not forward.
func Inbound(sessionID string, evt *events.Message, resolvePN func(types.JID) (types.JID, bool)) (p InboundPayload, media whatsmeow.DownloadableMessage, ok bool) {
	msg := evt.Message
	if evt.Info.IsFromMe || evt.IsEdit || msg == nil || !IsDirectChat(evt.Info.Chat) {
		return p, nil, false
	}
	if msg.GetProtocolMessage() != nil || msg.GetReactionMessage() != nil || msg.GetEncReactionMessage() != nil {
		return p, nil, false
	}
	content, ok := extractContent(msg)
	if !ok {
		return p, nil, false
	}

	p = InboundPayload{
		SessionID:        sessionID,
		ContentType:      content.contentType,
		Message:          content.text,
		MessageID:        evt.Info.ID,
		ReplyToMessageID: content.contextInfo.GetStanzaID(),
	}
	p.IsReply = p.ReplyToMessageID != ""
	p.FromNumber, p.FromLID = senderNumber(evt, resolvePN)

	if content.media != nil {
		media = content.media
		p.MediaMimetype = content.mimetype
		p.MediaFilename = content.filename
		if p.MediaFilename == "" {
			p.MediaFilename = evt.Info.ID + extensionFor(content.mimetype)
		}
	}
	return p, media, true
}

// senderNumber returns the digits to use as from_number and whether they are
// a raw LID (no phone number could be found).
func senderNumber(evt *events.Message, resolvePN func(types.JID) (types.JID, bool)) (digits string, isLID bool) {
	chat := evt.Info.Chat
	if chat.Server != types.HiddenUserServer {
		return chat.User, false
	}
	// RecipientAlt is our own side of the chat, so only SenderAlt is trusted.
	if alt := evt.Info.SenderAlt; alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.User, false
	}
	if resolvePN != nil {
		if pn, found := resolvePN(chat); found && pn.Server == types.DefaultUserServer && pn.User != "" {
			return pn.User, false
		}
	}
	return chat.User, true
}

type content struct {
	contentType string
	text        string
	contextInfo *waE2E.ContextInfo
	media       whatsmeow.DownloadableMessage
	mimetype    string
	filename    string
}

func extractContent(msg *waE2E.Message) (c content, ok bool) {
	switch {
	case msg.Conversation != nil:
		return content{contentType: "text", text: msg.GetConversation()}, true
	case msg.ExtendedTextMessage != nil:
		m := msg.ExtendedTextMessage
		return content{contentType: "text", text: m.GetText(), contextInfo: m.GetContextInfo()}, true
	case msg.ImageMessage != nil:
		m := msg.ImageMessage
		return content{"image", m.GetCaption(), m.GetContextInfo(), m, m.GetMimetype(), ""}, true
	case msg.VideoMessage != nil:
		m := msg.VideoMessage
		return content{"video", m.GetCaption(), m.GetContextInfo(), m, m.GetMimetype(), ""}, true
	case msg.AudioMessage != nil:
		m := msg.AudioMessage
		return content{"audio", "", m.GetContextInfo(), m, m.GetMimetype(), ""}, true
	case msg.DocumentMessage != nil:
		m := msg.DocumentMessage
		return content{"document", m.GetCaption(), m.GetContextInfo(), m, m.GetMimetype(), m.GetFileName()}, true
	case msg.StickerMessage != nil:
		return placeholder("[sticker]", msg.StickerMessage.GetContextInfo()), true
	case msg.LocationMessage != nil:
		return placeholder("[location]", msg.LocationMessage.GetContextInfo()), true
	case msg.LiveLocationMessage != nil:
		return placeholder("[location]", msg.LiveLocationMessage.GetContextInfo()), true
	case msg.ContactMessage != nil:
		return placeholder("[contact]", msg.ContactMessage.GetContextInfo()), true
	case msg.ContactsArrayMessage != nil:
		return placeholder("[contact]", msg.ContactsArrayMessage.GetContextInfo()), true
	case msg.PollCreationMessage != nil:
		return placeholder("[poll]", msg.PollCreationMessage.GetContextInfo()), true
	}
	if !hasUserContent(msg) {
		return content{}, false
	}
	return placeholder("[unsupported]", nil), true
}

// hasUserContent reports whether anything beyond transport metadata and
// non-chat control messages (poll votes, pins, keep-in-chat, event RSVPs,
// history notices, key distribution) is set.
func hasUserContent(msg *waE2E.Message) bool {
	rest := proto.Clone(msg).(*waE2E.Message)
	rest.MessageContextInfo = nil
	rest.SenderKeyDistributionMessage = nil
	rest.FastRatchetKeySenderKeyDistributionMessage = nil
	rest.PollUpdateMessage = nil
	rest.PollAddOptionMessage = nil
	rest.PinInChatMessage = nil
	rest.KeepInChatMessage = nil
	rest.EncEventResponseMessage = nil
	rest.SecretEncryptedMessage = nil
	rest.MessageHistoryBundle = nil
	rest.MessageHistoryNotice = nil
	rest.StickerSyncRmrMessage = nil
	rest.GroupRootKeyShare = nil
	rest.RootSecretDistributeMessage = nil
	return proto.Size(rest) > 0
}

func placeholder(text string, ci *waE2E.ContextInfo) content {
	return content{contentType: "text", text: text, contextInfo: ci}
}

// preferredExtensions pins the extension where mime.ExtensionsByType returns
// an obscure first entry (it sorts alphabetically and varies by host tables).
var preferredExtensions = map[string]string{
	"image/jpeg": ".jpg",
	"audio/ogg":  ".ogg",
	"audio/mpeg": ".mp3",
	"video/mp4":  ".mp4",
}

func extensionFor(mimetype string) string {
	base, _, err := mime.ParseMediaType(mimetype)
	if err != nil {
		base = mimetype
	}
	if ext, found := preferredExtensions[base]; found {
		return ext
	}
	if exts, err := mime.ExtensionsByType(base); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}
