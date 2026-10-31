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

// RotationGrace is how long the previous key stays valid after Rotate.
const RotationGrace = 10 * time.Minute

// MinKeyLen is the shortest API key a claim accepts.
const MinKeyLen = 32

var (
	ErrAlreadyClaimed    = errors.New("relay already claimed")
	ErrBadBootstrapToken = errors.New("bad bootstrap token")
	ErrBadRequest        = errors.New("bad claim request")
	// ErrEnvMode: an env-claimed relay takes its key and callback from env.
	ErrEnvMode   = errors.New("relay is claimed through env")
	ErrUnclaimed = errors.New("relay is not claimed")
	// ErrPopulatedUnclaimed stops a relay whose data dir holds linked numbers
	// but no claim from handing them to whoever claims it first.
	ErrPopulatedUnclaimed = errors.New("relay data dir has linked numbers but no claim; set RELAY_API_KEY or RELAY_BOOTSTRAP_TOKEN")
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
	// prevKey is the key replaced by the last Rotate, accepted until
	// prevExpiry. In memory only: after a restart the grace is gone, which
	// is safe because Frappe has the new key by then.
	prevKey    string
	prevExpiry time.Time
}

// New loads the relay's identity: env when cfg.APIKey is set, else the stored
// claim, else unclaimed. An unclaimed store that already holds data is
// refused unless a bootstrap token guards /claim.
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
		return s, nil
	}
	if cfg.BootstrapToken == "" {
		populated, err := st.HasAnyData(context.Background())
		if err != nil {
			return nil, fmt.Errorf("inspect data dir: %w", err)
		}
		if populated {
			return nil, ErrPopulatedUnclaimed
		}
	}
	return s, nil
}

func (s *State) Mode() Mode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

// Authorized reports, in constant time, whether key is the relay's API key,
// or the previous one still inside its rotation grace.
// An unclaimed relay authorizes nobody.
func (s *State) Authorized(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.mode == ModeUnclaimed {
		return false
	}
	ok := subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) == 1
	if s.prevKey != "" && s.now().Before(s.prevExpiry) &&
		subtle.ConstantTimeCompare([]byte(key), []byte(s.prevKey)) == 1 {
		ok = true
	}
	return ok
}

// Info is the claim data /info shows to an authenticated caller. No key.
func (s *State) Info() (callbackURL, siteName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.callbackURL, s.siteName
}

// Rotate replaces the API key. It is persisted first, then takes effect at
// once for callbacks; the old key keeps authorizing requests for RotationGrace.
func (s *State) Rotate(ctx context.Context, newKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireStored(); err != nil {
		return err
	}
	if err := validateKey(newKey); err != nil {
		return err
	}
	if err := s.st.UpdateRelayKey(ctx, newKey); err != nil {
		return fmt.Errorf("store key: %w", err)
	}
	s.prevKey, s.prevExpiry = s.apiKey, s.now().Add(RotationGrace)
	s.apiKey = newKey
	return nil
}

// SetCallback changes where callbacks go, with the same URL rules as Claim.
func (s *State) SetCallback(ctx context.Context, callbackURL, siteName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireStored(); err != nil {
		return err
	}
	if err := validateCallback(callbackURL); err != nil {
		return err
	}
	if err := s.st.UpdateRelayCallback(ctx, callbackURL, siteName); err != nil {
		return fmt.Errorf("store callback: %w", err)
	}
	s.callbackURL, s.siteName = callbackURL, siteName
	return nil
}

// requireStored: only a claimed (stored) identity can change. Caller holds mu.
func (s *State) requireStored() error {
	switch s.mode {
	case ModeEnv:
		return ErrEnvMode
	case ModeUnclaimed:
		return ErrUnclaimed
	}
	return nil
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

func validate(apiKey, callbackURL string) error {
	if err := validateKey(apiKey); err != nil {
		return err
	}
	return validateCallback(callbackURL)
}

// The validators never echo the key or the URL: errors may reach the caller and the log.
func validateKey(apiKey string) error {
	if len(apiKey) < MinKeyLen {
		return fmt.Errorf("%w: api_key must be at least %d characters", ErrBadRequest, MinKeyLen)
	}
	return nil
}

func validateCallback(callbackURL string) error {
	u, err := url.Parse(callbackURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: callback_url must be an http(s) URL", ErrBadRequest)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("%w: callback_url must not carry credentials, a query or a fragment", ErrBadRequest)
	}
	return nil
}
