package outbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// PingTimeout bounds the callback check made before a claim or callback change.
const PingTimeout = 10 * time.Second

const pingMethod = "ping"

// UnreachableError says why Frappe could not be reached at a callback URL.
// Detail is an HTTP status or an error class: never the key or the URL.
type UnreachableError struct{ Detail string }

func (e *UnreachableError) Error() string { return "callback unreachable: " + e.Detail }

// Ping proves that Frappe answers at baseURL for siteName and accepts apiKey,
// by POSTing to the key-verified api.ping method the way callbacks are sent.
func Ping(ctx context.Context, baseURL, siteName, apiKey string) error {
	ctx, cancel := context.WithTimeout(ctx, PingTimeout)
	defer cancel()
	url := strings.TrimRight(baseURL, "/") + "/api/method/yourwhatsapp.yourwhatsapp.api." + pingMethod
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return &UnreachableError{Detail: "bad request"}
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	if siteName != "" {
		req.Host = siteName
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return &UnreachableError{Detail: errorClass(err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &UnreachableError{Detail: fmt.Sprintf("http %d", resp.StatusCode)}
	}
	return nil
}

// errorClass names a transport failure without echoing it: the error text
// carries the URL, which may be what the caller is probing for.
func errorClass(err error) string {
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.As(err, &dnsErr):
		return "dns error"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "connection error"
	}
	return "request error"
}
