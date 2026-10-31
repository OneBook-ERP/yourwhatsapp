// Package claim owns the relay's identity: which key Frappe authenticates
// with and where callbacks go. The identity comes from env (RELAY_API_KEY),
// from a stored claim, or from the first successful POST /claim.
package claim

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"yourwhatsapp/relay/internal/config"
	"yourwhatsapp/relay/internal/store"
)

type Mode string

const (
	ModeEnv       Mode = "env"
	ModeClaimed   Mode = "claimed"
	ModeUnclaimed Mode = "unclaimed"
)

// MinKeyLen is the shortest API key a claim accepts.
const MinKeyLen = 32

var (
	ErrAlreadyClaimed    = errors.New("relay already claimed")
	ErrBadBootstrapToken = errors.New("bad bootstrap token")
	ErrBadRequest        = errors.New("bad claim request")
)

type State struct {
	st             *store.Store
	bootstrapToken string
	now            func() time.Time

	mu          sync.RWMutex
	mode        Mode
	apiKey      string
	callbackURL string
	siteName    string
	onClaimed   []func()
}

// New loads the relay's identity: env when cfg.APIKey is set, else the stored
// claim, else unclaimed.
func New(st *store.Store, cfg config.Config, now func() time.Time) (*State, error) {
	s := &State{st: st, bootstrapToken: cfg.BootstrapToken, now: now, mode: ModeUnclaimed}
	if cfg.APIKey != "" {
		s.mode, s.apiKey, s.callbackURL, s.siteName = ModeEnv, cfg.APIKey, cfg.FrappeBaseURL, cfg.FrappeSiteName
		return s, nil
	}
	rc, err := st.GetRelayConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("load relay config: %w", err)
	}
	if rc != nil {
		s.mode, s.apiKey, s.callbackURL, s.siteName = ModeClaimed, rc.APIKey, rc.CallbackURL, rc.SiteName
	}
	return s, nil
}

func (s *State) Mode() Mode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

// Authorized reports, in constant time, whether key is the relay's API key.
// An unclaimed relay authorizes nobody.
func (s *State) Authorized(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.mode == ModeUnclaimed {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) == 1
}

// Target is where callbacks go and the key they carry. All empty while unclaimed.
func (s *State) Target() (callbackURL, siteName, apiKey string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.callbackURL, s.siteName, s.apiKey
}

// OnClaimed registers fn to run once, after the next successful Claim.
func (s *State) OnClaimed(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onClaimed = append(s.onClaimed, fn)
}

// Claim gives an unclaimed relay its identity and persists it. Exactly one
// claim can ever succeed; the OnClaimed hooks run after it, outside the lock.
func (s *State) Claim(ctx context.Context, apiKey, callbackURL, siteName, bootstrapToken string) error {
	hooks, err := s.claim(ctx, apiKey, callbackURL, siteName, bootstrapToken)
	if err != nil {
		return err
	}
	for _, fn := range hooks {
		fn()
	}
	return nil
}

func (s *State) claim(ctx context.Context, apiKey, callbackURL, siteName, bootstrapToken string) ([]func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode != ModeUnclaimed {
		return nil, ErrAlreadyClaimed
	}
	if s.bootstrapToken != "" && subtle.ConstantTimeCompare([]byte(bootstrapToken), []byte(s.bootstrapToken)) != 1 {
		return nil, ErrBadBootstrapToken
	}
	if err := validate(apiKey, callbackURL); err != nil {
		return nil, err
	}
	rc := store.RelayConfig{APIKey: apiKey, CallbackURL: callbackURL, SiteName: siteName, ClaimedAt: s.now()}
	if err := s.st.InsertRelayConfig(ctx, rc); err != nil {
		if errors.Is(err, store.ErrRelayConfigExists) {
			return nil, ErrAlreadyClaimed
		}
		return nil, fmt.Errorf("store claim: %w", err)
	}
	s.mode, s.apiKey, s.callbackURL, s.siteName = ModeClaimed, apiKey, callbackURL, siteName
	hooks := s.onClaimed
	s.onClaimed = nil
	return hooks, nil
}

// validate never echoes the key: errors may reach the caller and the log.
func validate(apiKey, callbackURL string) error {
	if len(apiKey) < MinKeyLen {
		return fmt.Errorf("%w: api_key must be at least %d characters", ErrBadRequest, MinKeyLen)
	}
	u, err := url.Parse(callbackURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: callback_url must be an http(s) URL", ErrBadRequest)
	}
	return nil
}
