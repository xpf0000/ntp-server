package config

import (
	"fmt"
	"strings"
	"time"
)

type ClockMode string

const (
	ClockReal   ClockMode = "real"
	ClockOffset ClockMode = "offset"
	ClockFrozen ClockMode = "frozen"
)

// NTP timestamps represent the nearest point within half of a 32-bit seconds era.
const maxOffsetSeconds = (1 << 31) - 1

type ClockConfig struct {
	Mode          ClockMode `json:"mode"`
	OffsetSeconds int64     `json:"offsetSeconds,omitempty"`
	FrozenTime    string    `json:"frozenTime,omitempty"`
}

type ClockSettings struct {
	Mode       ClockMode
	Offset     time.Duration
	FrozenTime time.Time
}

func (c ClockConfig) Resolve() (ClockSettings, error) {
	mode := ClockMode(strings.ToLower(strings.TrimSpace(string(c.Mode))))
	switch mode {
	case "", ClockReal:
		return ClockSettings{Mode: ClockReal}, nil
	case ClockOffset:
		if c.OffsetSeconds < -maxOffsetSeconds || c.OffsetSeconds > maxOffsetSeconds {
			return ClockSettings{}, fmt.Errorf("clock.offsetSeconds must be between %d and %d", -maxOffsetSeconds, maxOffsetSeconds)
		}
		return ClockSettings{Mode: mode, Offset: time.Duration(c.OffsetSeconds) * time.Second}, nil
	case ClockFrozen:
		at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(c.FrozenTime))
		if err != nil {
			return ClockSettings{}, fmt.Errorf("clock.frozenTime must be an RFC 3339 timestamp with a timezone: %w", err)
		}
		return ClockSettings{Mode: mode, FrozenTime: at.UTC()}, nil
	default:
		return ClockSettings{}, fmt.Errorf("clock.mode %q must be real, offset, or frozen", c.Mode)
	}
}
