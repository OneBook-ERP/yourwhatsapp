package config

import (
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"RELAY_API_KEY", "FRAPPE_BASE_URL", "FRAPPE_SITE_NAME", "RELAY_HOST", "RELAY_PORT", "RELAY_DATA_DIR", "SEND_DELAY_MIN", "SEND_DELAY_MAX", "MEDIA_MAX_BYTES", "RELAY_BOOTSTRAP_TOKEN"} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELAY_API_KEY", "k")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 8800 || c.Host != "127.0.0.1" || c.DataDir != "./data" {
		t.Fatalf("bad defaults: %+v", c)
	}
	if c.SendDelayMin != 4*time.Second || c.SendDelayMax != 12*time.Second {
		t.Fatalf("bad delays: %v %v", c.SendDelayMin, c.SendDelayMax)
	}
	if c.MediaMaxBytes != 15728640 {
		t.Fatalf("bad media cap: %d", c.MediaMaxBytes)
	}
	if c.FrappeBaseURL != "http://127.0.0.1:8001" || c.APIKey != "k" {
		t.Fatalf("bad config: %+v", c)
	}
}

// Without RELAY_API_KEY the relay starts and waits to be claimed.
func TestLoadKeyOptional(t *testing.T) {
	clearEnv(t)
	if c, err := Load(); err != nil || c.APIKey != "" {
		t.Fatalf("Load without key = %+v, %v", c, err)
	}
}

func TestLoadRejectsInvertedDelays(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELAY_API_KEY", "k")
	t.Setenv("SEND_DELAY_MIN", "20s")
	t.Setenv("SEND_DELAY_MAX", "5s")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for inverted delays")
	}
}

func TestLoadMediaMaxBytes(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELAY_API_KEY", "k")
	t.Setenv("MEDIA_MAX_BYTES", "1048576")
	if c, err := Load(); err != nil || c.MediaMaxBytes != 1048576 {
		t.Fatalf("MediaMaxBytes = %d, %v", c.MediaMaxBytes, err)
	}
	for _, bad := range []string{"abc", "0", "-5"} {
		t.Setenv("MEDIA_MAX_BYTES", bad)
		if _, err := Load(); err == nil {
			t.Errorf("MEDIA_MAX_BYTES=%q: expected error", bad)
		}
	}
}

func TestLoadBootstrapToken(t *testing.T) {
	clearEnv(t)
	t.Setenv("RELAY_BOOTSTRAP_TOKEN", "boot")
	if c, err := Load(); err != nil || c.BootstrapToken != "boot" {
		t.Fatalf("BootstrapToken = %q, %v", c.BootstrapToken, err)
	}
}
