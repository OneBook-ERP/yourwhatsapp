package convert

import (
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// TextMessage builds a plain text message.
func TextMessage(text string) *waE2E.Message {
	return &waE2E.Message{Conversation: proto.String(text)}
}

// ContentToMediaType picks the upload media type for a content_type.
// Unknown types are uploaded as documents.
func ContentToMediaType(contentType string) whatsmeow.MediaType {
	switch contentType {
	case "image":
		return whatsmeow.MediaImage
	case "video":
		return whatsmeow.MediaVideo
	case "audio":
		return whatsmeow.MediaAudio
	}
	return whatsmeow.MediaDocument
}

// BuildMediaMessage wraps an uploaded attachment in the matching message type.
// Unknown content types are sent as documents.
func BuildMediaMessage(contentType, caption, mimetype, filename string, up whatsmeow.UploadResponse) *waE2E.Message {
	url, path, key := proto.String(up.URL), proto.String(up.DirectPath), up.MediaKey
	switch contentType {
	case "image":
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL: url, DirectPath: path, MediaKey: key, Mimetype: proto.String(mimetype),
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
			Caption: optionalString(caption),
		}}
	case "video":
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			URL: url, DirectPath: path, MediaKey: key, Mimetype: proto.String(mimetype),
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
			Caption: optionalString(caption),
		}}
	case "audio":
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			URL: url, DirectPath: path, MediaKey: key, Mimetype: proto.String(mimetype),
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
		}}
	}
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		URL: url, DirectPath: path, MediaKey: key, Mimetype: proto.String(mimetype),
		FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
		FileName: proto.String(filename), Caption: optionalString(caption),
	}}
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return proto.String(s)
}

// ReplyContext quotes the message targetID, written by participantJID, with
// quotedText shown in the quote bubble. No target id means no reply: nil.
func ReplyContext(targetID, participantJID, quotedText string) *waE2E.ContextInfo {
	if targetID == "" {
		return nil
	}
	ctx := &waE2E.ContextInfo{StanzaID: proto.String(targetID), Participant: optionalString(participantJID)}
	if quotedText != "" {
		ctx.QuotedMessage = &waE2E.Message{Conversation: proto.String(quotedText)}
	}
	return ctx
}

// WithReply attaches a reply context to msg. Plain text becomes extended
// text, the only text form that carries one. A nil ctx leaves msg as is.
func WithReply(msg *waE2E.Message, ctx *waE2E.ContextInfo) *waE2E.Message {
	if ctx == nil {
		return msg
	}
	switch {
	case msg.Conversation != nil:
		msg.ExtendedTextMessage = &waE2E.ExtendedTextMessage{Text: msg.Conversation, ContextInfo: ctx}
		msg.Conversation = nil
	case msg.ExtendedTextMessage != nil:
		msg.ExtendedTextMessage.ContextInfo = ctx
	case msg.ImageMessage != nil:
		msg.ImageMessage.ContextInfo = ctx
	case msg.VideoMessage != nil:
		msg.VideoMessage.ContextInfo = ctx
	case msg.AudioMessage != nil:
		msg.AudioMessage.ContextInfo = ctx
	case msg.DocumentMessage != nil:
		msg.DocumentMessage.ContextInfo = ctx
	}
	return msg
}
