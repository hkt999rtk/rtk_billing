package main

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
)

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

func TestStartupMigrationRespectsRuntimeGate(t *testing.T) {
	for _, tc := range []struct {
		name, value    string
		wantCall       bool
		migrationError error
		wantError      bool
	}{
		{name: "restricted runtime", value: "false"},
		{name: "development default", wantCall: true},
		{name: "enabled", value: "true", wantCall: true},
		{name: "migration failure", value: "true", wantCall: true, migrationError: errors.New("migration denied"), wantError: true},
		{name: "invalid setting", value: "flase", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var db pgxpool.Pool
			called := false
			err := migrateOnStartup(ctx, &db, tc.value, func(gotCtx context.Context, gotDB *pgxpool.Pool) error {
				called = true
				if gotCtx != ctx || gotDB != &db {
					t.Fatal("migration received different startup context or database")
				}
				return tc.migrationError
			})
			if called != tc.wantCall || (err != nil) != tc.wantError {
				t.Fatalf("called=%t, err=%v", called, err)
			}
			if tc.migrationError != nil && !errors.Is(err, tc.migrationError) {
				t.Fatalf("migration error lost: %v", err)
			}
		})
	}
}
