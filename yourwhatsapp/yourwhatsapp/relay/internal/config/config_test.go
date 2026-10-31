package config

import (
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"RELAY_API_KEY", "FRAPPE_BASE_URL", "FRAPPE_SITE_NAME", "RELAY_HOST", "RELAY_PORT", "RELAY_DATA_DIR", "SEND_DELAY_MIN", "SEND_DELAY_MAX"} {
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
	if c.FrappeBaseURL != "http://127.0.0.1:8001" || c.APIKey != "k" {
		t.Fatalf("bad config: %+v", c)
	}
}

func TestLoadRequiresKey(t *testing.T) {
	clearEnv(t)
	if _, err := Load(); err == nil {
		t.Fatal("expected error for missing key")
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
