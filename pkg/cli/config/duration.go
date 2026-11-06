package config

import (
	"fmt"
	"os"
	"time"
)

const (
	// EnvReaperQuarantine is how long the reaper leaves an unowned Oxide resource alone
	// before deleting it.
	EnvReaperQuarantine = "OXIDE_REAPER_QUARANTINE"
	// EnvRepairToleration is how long a node may stay NotReady before it is rebooted.
	EnvRepairToleration = "OXIDE_REPAIR_TOLERATION"
)

// DurationFromEnv reads a positive duration from the environment, or fallback when unset.
func DurationFromEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid duration: %w", name, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", name, d)
	}
	return d, nil
}
