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

type frappePoster struct {
	client   *http.Client
	baseURL  string
	siteName string
	apiKey   string
}

func NewFrappePoster(baseURL, siteName, apiKey string) Poster {
	return &frappePoster{
		client: &http.Client{
			Timeout: 30 * time.Second,
			// A redirect (e.g. to a login page) is an answer, not a hop: it
			// must not turn a POST into a GET that "succeeds" with 200.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		baseURL:  strings.TrimRight(baseURL, "/"),
		siteName: siteName,
		apiKey:   apiKey,
	}
}

func (p *frappePoster) Post(ctx context.Context, method string, payload []byte) (int, error) {
	url := p.baseURL + "/api/method/yourwhatsapp.yourwhatsapp.api." + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Api-Key", p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	if p.siteName != "" {
		req.Host = p.siteName
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, nil
}
