package main

import (
	"strings"
	"testing"
	"time"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/providers/instance"
)

func TestShapeConfigFromEnv(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv(envInstanceTypeIncrementPercent, "")
		cfg, err := shapeConfigFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.IncrementPercent != instancetype.DefaultShapeIncrementPercent {
			t.Errorf(
				"increment = %v, want %v",
				cfg.IncrementPercent,
				instancetype.DefaultShapeIncrementPercent,
			)
		}
	})

	t.Run("configured", func(t *testing.T) {
		t.Setenv(envInstanceTypeIncrementPercent, "25")
		cfg, err := shapeConfigFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.IncrementPercent != 25 {
			t.Errorf("increment = %v, want 25", cfg.IncrementPercent)
		}
	})

	for _, value := range []string{"not-a-number", "0", "101", "NaN"} {
		t.Run("invalid "+value, func(t *testing.T) {
			t.Setenv(envInstanceTypeIncrementPercent, value)
			_, err := shapeConfigFromEnv()
			if err == nil || !strings.Contains(err.Error(), envInstanceTypeIncrementPercent) {
				t.Fatalf("error = %v, want one naming %s", err, envInstanceTypeIncrementPercent)
			}
		})
	}
}

func TestDeadlinesFromEnv(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		deadlines, err := deadlinesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if deadlines != instance.DefaultDeadlines() {
			t.Errorf("deadlines = %+v, want the defaults", deadlines)
		}
	})

	t.Run("configured", func(t *testing.T) {
		t.Setenv(envLaunchDiskCreatingDeadline, "45s")
		deadlines, err := deadlinesFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if deadlines.DiskCreating != 45*time.Second {
			t.Errorf("disk creating deadline = %s, want 45s", deadlines.DiskCreating)
		}
	})

	t.Run("unparseable", func(t *testing.T) {
		t.Setenv(envLaunchDeadline, "ten minutes")
		if _, err := deadlinesFromEnv(); err == nil {
			t.Error("deadlinesFromEnv() accepted an unparseable duration, want it refused")
		}
	})

	t.Run("inconsistent", func(t *testing.T) {
		t.Setenv(envLaunchDeadline, "1m")
		t.Setenv(envLaunchInstanceCreatingDeadline, "5m")
		if _, err := deadlinesFromEnv(); err == nil {
			t.Error(
				"deadlinesFromEnv() accepted a deadline longer than the launch, want it refused",
			)
		}
	})
}

func TestReaperConfigFromEnv(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		cfg, err := reaperConfigFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.enabled {
			t.Error("enabled = false, want the reaper on unless it is switched off")
		}
		if cfg.period != defaultReaperPeriod || cfg.quarantine != defaultReaperQuarantine {
			t.Errorf("period = %s, quarantine = %s, want the defaults", cfg.period, cfg.quarantine)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		t.Setenv(envReaperEnabled, "false")
		cfg, err := reaperConfigFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.enabled {
			t.Error("enabled = true, want the reaper switched off")
		}
	})

	t.Run("invalid", func(t *testing.T) {
		t.Setenv(envReaperQuarantine, "0s")
		if _, err := reaperConfigFromEnv(); err == nil {
			t.Error("reaperConfigFromEnv() accepted a zero quarantine, want it refused")
		}
	})
}
