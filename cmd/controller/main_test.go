package main

import (
	"strings"
	"testing"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
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
