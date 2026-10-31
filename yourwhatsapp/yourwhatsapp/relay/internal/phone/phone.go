// Package phone normalizes user-supplied phone numbers to bare digits.
package phone

import "errors"

const (
	minDigits = 8
	maxDigits = 15
)

var ErrInvalid = errors.New("invalid phone number")

// Normalize strips everything except ASCII digits and requires 8-15 of them.
func Normalize(raw string) (string, error) {
	digits := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] >= '0' && raw[i] <= '9' {
			digits = append(digits, raw[i])
		}
	}
	if len(digits) < minDigits || len(digits) > maxDigits {
		return "", ErrInvalid
	}
	return string(digits), nil
}
