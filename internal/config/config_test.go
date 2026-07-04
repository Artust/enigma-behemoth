package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("MAX_DAMAGE_PER_HIT", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr default = %q", c.HTTPAddr)
	}
	if c.BatchMaxWait != 10*time.Millisecond {
		t.Errorf("BatchMaxWait default = %v", c.BatchMaxWait)
	}
	if c.MaxDamagePerHit <= 0 {
		t.Errorf("MaxDamagePerHit default should be positive, got %d", c.MaxDamagePerHit)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("HTTP_ADDR", ":9999")
	t.Setenv("BATCH_MAX_WAIT", "12ms")
	t.Setenv("MAX_DAMAGE_PER_HIT", "42")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTPAddr != ":9999" {
		t.Errorf("HTTPAddr = %q, want :9999", c.HTTPAddr)
	}
	if c.BatchMaxWait != 12*time.Millisecond {
		t.Errorf("BatchMaxWait = %v, want 12ms", c.BatchMaxWait)
	}
	if c.MaxDamagePerHit != 42 {
		t.Errorf("MaxDamagePerHit = %d, want 42", c.MaxDamagePerHit)
	}
}

// TestLoadRejectsInvalid: Load returns a validation error for a bad damage cap or zero queue/batch.
func TestLoadRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"non-positive max damage", "MAX_DAMAGE_PER_HIT", "-1"},
		{"zero max damage", "MAX_DAMAGE_PER_HIT", "0"},
		{"zero writer queue", "WRITER_QUEUE_SIZE", "0"},
		{"zero batch size", "BATCH_MAX_SIZE", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(c.key, c.val)
			if _, err := Load(); err == nil {
				t.Fatalf("Load with %s=%q returned nil error, want a validation error", c.key, c.val)
			}
		})
	}
}

// TestLoadMalformedEnvFallsBackToDefault: garbage env values fall back to defaults, not zero.
func TestLoadMalformedEnvFallsBackToDefault(t *testing.T) {
	t.Setenv("BATCH_MAX_WAIT", "not-a-duration")
	t.Setenv("WRITER_QUEUE_SIZE", "not-an-int")
	t.Setenv("MAX_DAMAGE_PER_HIT", "not-an-int64")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load with malformed env should fall back to defaults, got err: %v", err)
	}
	if c.BatchMaxWait != 10*time.Millisecond {
		t.Errorf("BatchMaxWait = %v, want default 10ms after malformed value", c.BatchMaxWait)
	}
	if c.WriterQueueSize != 20_000 {
		t.Errorf("WriterQueueSize = %d, want default 20000 after malformed value", c.WriterQueueSize)
	}
	if c.MaxDamagePerHit != 1_000_000_000 {
		t.Errorf("MaxDamagePerHit = %d, want default 1e9 after malformed value", c.MaxDamagePerHit)
	}
}
