// ota-cutover-audit inventories the profile-local to UTC month boundary without
// changing pricing, usage, billing periods, or invoices.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
	"github.com/hkt999rtk/rtk_billing/internal/database"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("ota-cutover-audit", flag.ContinueOnError)
	effective := flags.String("effective-from", "", "proposed future UTC month start, RFC3339")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*effective) == "" {
		return errors.New("--effective-from is required")
	}
	cutover, err := time.Parse(time.RFC3339, *effective)
	if err != nil {
		return errors.New("--effective-from must be RFC3339")
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		return errors.New("database connection failed")
	}
	defer db.Close()
	report, err := billingstore.New(db).AuditOTACutover(ctx, cutover)
	if err != nil {
		return fmt.Errorf("read-only OTA cutover audit failed: %w", err)
	}
	return json.NewEncoder(output).Encode(report)
}
