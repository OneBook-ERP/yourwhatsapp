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
	APIKey         string
	FrappeBaseURL  string
	FrappeSiteName string
	Host           string
	Port           int
	DataDir        string
	SendDelayMin   time.Duration
	SendDelayMax   time.Duration
}

func Load() (Config, error) {
	c := Config{
		APIKey:         os.Getenv("RELAY_API_KEY"),
		FrappeBaseURL:  envOr("FRAPPE_BASE_URL", "http://127.0.0.1:8001"),
		FrappeSiteName: os.Getenv("FRAPPE_SITE_NAME"),
		Host:           envOr("RELAY_HOST", "127.0.0.1"),
		DataDir:        envOr("RELAY_DATA_DIR", "./data"),
	}
	if c.APIKey == "" {
		return Config{}, errors.New("RELAY_API_KEY is required")
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
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
