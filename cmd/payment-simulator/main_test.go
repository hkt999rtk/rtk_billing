package main

import "testing"

func TestMigrationStartupGate(t *testing.T) {
	for _, tc := range []struct {
		value       string
		wantMigrate bool
		wantError   bool
	}{
		{value: "", wantMigrate: true},
		{value: "true", wantMigrate: true},
		{value: "false"},
		{value: " false "},
		{value: "flase", wantError: true},
	} {
		got, err := shouldMigrateOnStartup(tc.value)
		if (err != nil) != tc.wantError || got != tc.wantMigrate {
			t.Errorf("value %q: migrate=%t, err=%v", tc.value, got, err)
		}
	}
}
