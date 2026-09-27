// migrate applies Billing schema changes without starting the HTTP server.
// Protected environments run this binary with a dedicated migration identity
// before rolling out API and worker workloads.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hkt999rtk/rtk_billing/internal/database"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	url := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if url == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := database.Connect(ctx, url)
	if err != nil {
		return errors.New("billing migration database connection failed")
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		return fmt.Errorf("billing migration failed: %w", err)
	}
	fmt.Println("billing migrations applied")
	return nil
}
