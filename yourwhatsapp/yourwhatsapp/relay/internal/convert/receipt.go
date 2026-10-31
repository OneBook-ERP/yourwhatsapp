// Package convert maps whatsmeow events and messages to and from the relay's
// Frappe payloads. It holds pure functions only.
package convert

import "go.mau.fi/whatsmeow/types"

// ReceiptStatus maps a whatsmeow receipt type to the status Frappe stores.
// Receipt types that carry no delivery information report ok=false.
func ReceiptStatus(t types.ReceiptType) (status string, ok bool) {
	switch t {
	case types.ReceiptTypeDelivered:
		return "Delivered", true
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf, types.ReceiptTypePlayed:
		return "Read", true
	}
	return "", false
}
