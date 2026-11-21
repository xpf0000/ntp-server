package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name, input, field string
	}{
		{"missing NTP port", `{"ntp":{"listenAddress":"127.0.0.1"}}`, "ntp.listenAddress"},
		{"invalid API port", `{"api":{"listenAddress":"127.0.0.1:70000"}}`, "api.listenAddress"},
		{"invalid mode", `{"clock":{"mode":"unknown"}}`, "clock.mode"},
		{"missing frozen time", `{"clock":{"mode":"frozen"}}`, "clock.frozenTime"},
		{"missing timezone", `{"clock":{"mode":"frozen","frozenTime":"2038-01-19T12:00:00"}}`, "clock.frozenTime"},
		{"offset outside NTP era window", `{"clock":{"mode":"offset","offsetSeconds":2147483648}}`, "clock.offsetSeconds"},
		{"negative offset outside NTP era window", `{"clock":{"mode":"offset","offsetSeconds":-2147483648}}`, "clock.offsetSeconds"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.input), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("expected an error naming %s, got %v", tc.field, err)
			}
		})
	}
}
