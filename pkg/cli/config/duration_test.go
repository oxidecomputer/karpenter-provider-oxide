package config

import (
	"testing"
	"time"
)

func TestDurationFromEnv(t *testing.T) {
	const name = "OXIDE_TEST_DURATION"
	t.Run("unset returns the fallback", func(t *testing.T) {
		t.Setenv(name, "")
		d, err := DurationFromEnv(name, time.Minute)
		if err != nil || d != time.Minute {
			t.Fatalf("got %s, %v", d, err)
		}
	})
	t.Run("set overrides the fallback", func(t *testing.T) {
		t.Setenv(name, "90s")
		d, err := DurationFromEnv(name, time.Minute)
		if err != nil || d != 90*time.Second {
			t.Fatalf("got %s, %v", d, err)
		}
	})
	for _, value := range []string{"soon", "0", "-1m"} {
		t.Run("rejects "+value, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := DurationFromEnv(name, time.Minute); err == nil {
				t.Fatal("no error")
			}
		})
	}
}
