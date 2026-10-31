package convert

import (
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func pn(user string) types.JID  { return types.NewJID(user, types.DefaultUserServer) }
func lid(user string) types.JID { return types.NewJID(user, types.HiddenUserServer) }

func msgEvt(chat types.JID, m *waE2E.Message) *events.Message {
	e := &events.Message{Message: m}
	e.Info.ID = "ABC123"
	e.Info.Chat = chat
	e.Info.Sender = chat
	return e
}

func noResolve(types.JID) (types.JID, bool) { return types.JID{}, false }

func TestInboundText(t *testing.T) {
	p, media, ok := Inbound("s1", msgEvt(pn("919876543210"), &waE2E.Message{Conversation: proto.String("hi")}), noResolve)
	if !ok || media != nil {
		t.Fatalf("ok=%v media=%v", ok, media)
	}
	if p.SessionID != "s1" || p.FromNumber != "919876543210" || p.ContentType != "text" ||
		p.Message != "hi" || p.MessageID != "ABC123" || p.IsReply || p.FromLID {
		t.Fatalf("payload: %+v", p)
	}
}

func TestInboundExtendedText(t *testing.T) {
	m := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("link https://x.y")}}
	p, _, ok := Inbound("s1", msgEvt(pn("1"), m), noResolve)
	if !ok || p.Message != "link https://x.y" || p.ContentType != "text" {
		t.Fatalf("ok=%v %+v", ok, p)
	}
}

func TestInboundDropsFromMe(t *testing.T) {
	e := msgEvt(pn("1"), &waE2E.Message{Conversation: proto.String("hi")})
	e.Info.IsFromMe = true
	if _, _, ok := Inbound("s1", e, noResolve); ok {
		t.Fatal("expected drop")
	}
}

func TestInboundDropsNonDirectChats(t *testing.T) {
	m := &waE2E.Message{Conversation: proto.String("hi")}
	cases := map[string]types.JID{
		"group":      types.NewJID("123-456", types.GroupServer),
		"status":     types.StatusBroadcastJID,
		"broadcast":  types.NewJID("abc", types.BroadcastServer),
		"newsletter": types.NewJID("abc", types.NewsletterServer),
	}
	for name, chat := range cases {
		if _, _, ok := Inbound("s1", msgEvt(chat, m), noResolve); ok {
			t.Errorf("%s: expected drop", name)
		}
	}
}

func TestInboundResolvedLID(t *testing.T) {
	resolve := func(j types.JID) (types.JID, bool) {
		if j.User == "999" {
			return pn("919876543210"), true
		}
		return types.JID{}, false
	}
	p, _, ok := Inbound("s1", msgEvt(lid("999"), &waE2E.Message{Conversation: proto.String("hi")}), resolve)
	if !ok || p.FromNumber != "919876543210" || p.FromLID {
		t.Fatalf("ok=%v %+v", ok, p)
	}
}

func TestInboundLIDUsesSenderAltBeforeResolver(t *testing.T) {
	e := msgEvt(lid("999"), &waE2E.Message{Conversation: proto.String("hi")})
	e.Info.SenderAlt = pn("447700900123")
	p, _, ok := Inbound("s1", e, func(types.JID) (types.JID, bool) { t.Fatal("resolver called"); return types.JID{}, false })
	if !ok || p.FromNumber != "447700900123" || p.FromLID {
		t.Fatalf("ok=%v %+v", ok, p)
	}
}

func TestInboundUnresolvedLID(t *testing.T) {
	p, _, ok := Inbound("s1", msgEvt(lid("999"), &waE2E.Message{Conversation: proto.String("hi")}), noResolve)
	if !ok || p.FromNumber != "999" || !p.FromLID {
		t.Fatalf("ok=%v %+v", ok, p)
	}
}

func TestInboundImageWithCaption(t *testing.T) {
	m := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("look"), Mimetype: proto.String("image/jpeg")}}
	p, media, ok := Inbound("s1", msgEvt(pn("1"), m), noResolve)
	if !ok || media == nil {
		t.Fatalf("ok=%v media=%v", ok, media)
	}
	if p.ContentType != "image" || p.Message != "look" || p.MediaMimetype != "image/jpeg" || p.MediaFilename != "ABC123.jpg" || p.MediaBase64 != "" {
		t.Fatalf("payload: %+v", p)
	}
}

func TestInboundUnknownMimetypeFilenameFallsBackToBin(t *testing.T) {
	m := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("application/x-nothing")}}
	p, _, _ := Inbound("s1", msgEvt(pn("1"), m), noResolve)
	if p.MediaFilename != "ABC123.bin" {
		t.Fatalf("filename %q", p.MediaFilename)
	}
}

func TestInboundDocumentKeepsFileName(t *testing.T) {
	m := &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		FileName: proto.String("invoice.pdf"), Mimetype: proto.String("application/pdf"), Caption: proto.String("see"),
	}}
	p, media, ok := Inbound("s1", msgEvt(pn("1"), m), noResolve)
	if !ok || media == nil || p.ContentType != "document" || p.MediaFilename != "invoice.pdf" || p.Message != "see" {
		t.Fatalf("ok=%v %+v", ok, p)
	}
}

func TestInboundVoiceNoteIsAudio(t *testing.T) {
	m := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Mimetype: proto.String("audio/ogg; codecs=opus")}}
	p, media, ok := Inbound("s1", msgEvt(pn("1"), m), noResolve)
	if !ok || media == nil || p.ContentType != "audio" || p.MediaMimetype != "audio/ogg; codecs=opus" {
		t.Fatalf("ok=%v %+v", ok, p)
	}
}

func TestInboundQuotedReply(t *testing.T) {
	m := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text:        proto.String("yes"),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("ORIG1")},
	}}
	p, _, ok := Inbound("s1", msgEvt(pn("1"), m), noResolve)
	if !ok || !p.IsReply || p.ReplyToMessageID != "ORIG1" {
		t.Fatalf("ok=%v %+v", ok, p)
	}
}

func TestInboundStickerPlaceholder(t *testing.T) {
	p, media, ok := Inbound("s1", msgEvt(pn("1"), &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}), noResolve)
	if !ok || media != nil || p.ContentType != "text" || p.Message != "[sticker]" {
		t.Fatalf("ok=%v %+v", ok, p)
	}
}

func TestInboundOtherPlaceholders(t *testing.T) {
	cases := map[string]*waE2E.Message{
		"[location]":    {LocationMessage: &waE2E.LocationMessage{}},
		"[contact]":     {ContactMessage: &waE2E.ContactMessage{}},
		"[poll]":        {PollCreationMessage: &waE2E.PollCreationMessage{}},
		"[unsupported]": {ListMessage: &waE2E.ListMessage{}},
	}
	for want, m := range cases {
		p, _, ok := Inbound("s1", msgEvt(pn("1"), m), noResolve)
		if !ok || p.Message != want {
			t.Errorf("%s: ok=%v %+v", want, ok, p)
		}
	}
}

func TestInboundDropsReaction(t *testing.T) {
	m := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}
	if _, _, ok := Inbound("s1", msgEvt(pn("1"), m), noResolve); ok {
		t.Fatal("expected drop")
	}
}

func TestInboundDropsProtocolEditAndEmpty(t *testing.T) {
	proto1 := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{}}
	if _, _, ok := Inbound("s1", msgEvt(pn("1"), proto1), noResolve); ok {
		t.Error("protocol: expected drop")
	}
	edit := msgEvt(pn("1"), &waE2E.Message{Conversation: proto.String("fixed")})
	edit.IsEdit = true
	if _, _, ok := Inbound("s1", edit, noResolve); ok {
		t.Error("edit: expected drop")
	}
	empty := &waE2E.Message{MessageContextInfo: &waE2E.MessageContextInfo{}}
	if _, _, ok := Inbound("s1", msgEvt(pn("1"), empty), noResolve); ok {
		t.Error("empty: expected drop")
	}
}

func TestReceiptStatus(t *testing.T) {
	cases := []struct {
		in     types.ReceiptType
		want   string
		wantOK bool
	}{
		{types.ReceiptTypeDelivered, "Delivered", true},
		{types.ReceiptTypeRead, "Read", true},
		{types.ReceiptTypeReadSelf, "Read", true},
		{types.ReceiptTypePlayed, "Read", true},
		{types.ReceiptTypeSender, "", false},
		{types.ReceiptTypeRetry, "", false},
		{types.ReceiptTypeServerError, "", false},
	}
	for _, c := range cases {
		got, ok := ReceiptStatus(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("%q: got (%q,%v) want (%q,%v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestContentToMediaType(t *testing.T) {
	cases := map[string]whatsmeow.MediaType{
		"image": whatsmeow.MediaImage, "video": whatsmeow.MediaVideo,
		"audio": whatsmeow.MediaAudio, "document": whatsmeow.MediaDocument, "weird": whatsmeow.MediaDocument,
	}
	for in, want := range cases {
		if got := ContentToMediaType(in); got != want {
			t.Errorf("%s: got %v want %v", in, got, want)
		}
	}
}

func TestBuildMediaMessage(t *testing.T) {
	up := whatsmeow.UploadResponse{URL: "u", DirectPath: "/d", MediaKey: []byte{1}, FileLength: 5}
	img := BuildMediaMessage("image", "cap", "image/png", "a.png", up)
	if img.ImageMessage == nil || img.ImageMessage.GetCaption() != "cap" || img.ImageMessage.GetURL() != "u" || img.ImageMessage.GetFileLength() != 5 {
		t.Fatalf("image: %+v", img)
	}
	if BuildMediaMessage("video", "", "video/mp4", "a.mp4", up).VideoMessage == nil {
		t.Error("video")
	}
	if BuildMediaMessage("audio", "", "audio/ogg", "a.ogg", up).AudioMessage == nil {
		t.Error("audio")
	}
}

func TestBuildMediaMessageDocumentFallback(t *testing.T) {
	m := BuildMediaMessage("sticker", "cap", "application/pdf", "x.pdf", whatsmeow.UploadResponse{})
	d := m.DocumentMessage
	if d == nil || d.GetFileName() != "x.pdf" || d.GetCaption() != "cap" || d.GetMimetype() != "application/pdf" {
		t.Fatalf("doc: %+v", m)
	}
}

func TestTextMessage(t *testing.T) {
	if TextMessage("yo").GetConversation() != "yo" {
		t.Fatal("text")
	}
}

func TestIsDirectChat(t *testing.T) {
	if !IsDirectChat(pn("1")) || !IsDirectChat(lid("1")) || IsDirectChat(types.NewJID("1", types.GroupServer)) {
		t.Fatal("IsDirectChat")
	}
}

func TestInboundLIDIgnoresRecipientAlt(t *testing.T) {
	e := msgEvt(lid("999"), &waE2E.Message{Conversation: proto.String("hi")})
	e.Info.RecipientAlt = pn("919000000000")
	p, _, ok := Inbound("s1", e, noResolve)
	if !ok || p.FromNumber != "999" || !p.FromLID {
		t.Fatalf("ok=%v %+v", ok, p)
	}
}

func TestInboundLIDNilResolver(t *testing.T) {
	p, _, ok := Inbound("s1", msgEvt(lid("999"), &waE2E.Message{Conversation: proto.String("hi")}), nil)
	if !ok || p.FromNumber != "999" || !p.FromLID {
		t.Fatalf("ok=%v %+v", ok, p)
	}
}
