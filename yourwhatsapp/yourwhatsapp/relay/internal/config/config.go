// Package config loads relay settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	// APIKey, when set, pins the relay's identity to env (mode "env"); when
	// empty the relay uses its stored claim or waits for POST /claim.
	APIKey string
	// BootstrapToken, when set, must accompany POST /claim.
	BootstrapToken string
	FrappeBaseURL  string
	FrappeSiteName string
	Host           string
	Port           int
	DataDir        string
	SendDelayMin   time.Duration
	SendDelayMax   time.Duration
	// MediaMaxBytes caps inbound media the relay downloads and forwards.
	MediaMaxBytes int64
}

func Load() (Config, error) {
	c := Config{
		APIKey:         os.Getenv("RELAY_API_KEY"),
		BootstrapToken: os.Getenv("RELAY_BOOTSTRAP_TOKEN"),
		FrappeBaseURL:  envOr("FRAPPE_BASE_URL", "http://127.0.0.1:8001"),
		FrappeSiteName: os.Getenv("FRAPPE_SITE_NAME"),
		Host:           envOr("RELAY_HOST", "127.0.0.1"),
		DataDir:        envOr("RELAY_DATA_DIR", "./data"),
	}
	port, err := strconv.Atoi(envOr("RELAY_PORT", "8800"))
	if err != nil {
		return Config{}, fmt.Errorf("RELAY_PORT: %w", err)
	}
	c.Port = port

	if c.SendDelayMin, err = time.ParseDuration(envOr("SEND_DELAY_MIN", "4s")); err != nil {
		return Config{}, fmt.Errorf("SEND_DELAY_MIN: %w", err)
	}
	if c.SendDelayMax, err = time.ParseDuration(envOr("SEND_DELAY_MAX", "12s")); err != nil {
		return Config{}, fmt.Errorf("SEND_DELAY_MAX: %w", err)
	}
	if c.SendDelayMin > c.SendDelayMax {
		return Config{}, errors.New("SEND_DELAY_MIN must not exceed SEND_DELAY_MAX")
	}
	if c.MediaMaxBytes, err = strconv.ParseInt(envOr("MEDIA_MAX_BYTES", "15728640"), 10, 64); err != nil {
		return Config{}, fmt.Errorf("MEDIA_MAX_BYTES: %w", err)
	}
	if c.MediaMaxBytes <= 0 {
		return Config{}, errors.New("MEDIA_MAX_BYTES must be positive")
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
