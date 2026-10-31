package outbox

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	MethodInbound      = "inbound_message"
	MethodStatus       = "message_status"
	MethodQR           = "update_qr"
	MethodAccountEvent = "account_event"
)

// Poster delivers one callback. A non-nil err means no HTTP response was received.
type Poster interface {
	Post(ctx context.Context, method string, payload []byte) (status int, err error)
}

// TargetFunc returns where callbacks go: Frappe's base URL, the site name sent
// as Host (empty: the URL's host) and the API key.
type TargetFunc func() (baseURL, siteName, apiKey string)

type frappePoster struct {
	client *http.Client
	target TargetFunc
}

// NewFrappePoster reads target on every post, so a new claim or key applies
// to the next callback.
func NewFrappePoster(target TargetFunc) Poster {
	return &frappePoster{
		client: &http.Client{
			Timeout: 30 * time.Second,
			// A redirect (e.g. to a login page) is an answer, not a hop: it
			// must not turn a POST into a GET that "succeeds" with 200.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		target: target,
	}
}

func (p *frappePoster) Post(ctx context.Context, method string, payload []byte) (int, error) {
	baseURL, siteName, apiKey := p.target()
	url := strings.TrimRight(baseURL, "/") + "/api/method/yourwhatsapp.yourwhatsapp.api." + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	if siteName != "" {
		req.Host = siteName
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, nil
}
