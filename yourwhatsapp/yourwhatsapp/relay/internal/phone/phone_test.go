package phone

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{"+91 98765 43210", "919876543210", false},
		{"(415) 555-0100", "4155550100", false},
		{"1234567", "", true},
		{strings.Repeat("1", 16), "", true},
		{"", "", true},
	}
	for _, tc := range tests {
		got, err := Normalize(tc.in)
		if tc.wantErr {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Normalize(%q) err = %v, want ErrInvalid", tc.in, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}
