package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/oxidecomputer/karpenter-provider-oxide/pkg/instancetype"
)

const EnvInstanceTypeIncrementPercent = "OXIDE_INSTANCE_TYPE_INCREMENT_PERCENT"

func ShapeConfigFromEnv() (instancetype.ShapeConfig, error) {
	cfg := instancetype.DefaultShapeConfig()
	raw := os.Getenv(EnvInstanceTypeIncrementPercent)
	if raw == "" {
		return cfg, nil
	}

	increment, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return instancetype.ShapeConfig{}, fmt.Errorf(
			"%s is not a valid percentage: %w", EnvInstanceTypeIncrementPercent, err,
		)
	}
	cfg.IncrementPercent = increment
	if err := cfg.Validate(); err != nil {
		return instancetype.ShapeConfig{}, fmt.Errorf(
			"invalid %s: %w", EnvInstanceTypeIncrementPercent, err,
		)
	}
	return cfg, nil
}
