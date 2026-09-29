// ota-cutover-bridge previews and closes the final local month at a published
// OTA UTC boundary. It never publishes pricing or collects a payment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/hkt999rtk/rtk_billing/internal/billingstore"
	"github.com/hkt999rtk/rtk_billing/internal/database"
	"io"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string, out io.Writer) error {
	f := flag.NewFlagSet("ota-cutover-bridge", flag.ContinueOnError)
	org := f.String("organization", "", "Brand Cloud UUID")
	version := f.String("pricing-version", "", "published OTA pricing version UUID")
	apply := f.Bool("apply", false, "issue the reviewed final local-month invoice")
	digest := f.String("review-sha256", "", "digest returned by the read-only preview")
	actor := f.String("created-by", "", "operator identity for the immutable receipt")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || strings.TrimSpace(*org) == "" || strings.TrimSpace(*version) == "" {
		return errors.New("--organization and --pricing-version are required")
	}
	if *apply && (len(*digest) != 64 || strings.TrimSpace(*actor) == "") {
		return errors.New("--apply requires --review-sha256 and --created-by")
	}
	if !*apply && (*digest != "" || *actor != "") {
		return errors.New("review digest and operator apply only with --apply")
	}
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, dsn)
	if err != nil {
		return errors.New("database connection failed")
	}
	defer db.Close()
	store := billingstore.New(db)
	if !*apply {
		review, err := store.ReviewOTACutoverBridge(ctx, *org, *version)
		if err != nil {
			return fmt.Errorf("bridge review failed: %w", err)
		}
		return json.NewEncoder(out).Encode(review)
	}
	invoice, created, err := store.ApplyOTACutoverBridge(ctx, billingstore.OTACutoverBridgeInput{OrganizationID: *org, PricingVersionID: *version, ReviewSHA256: *digest, CreatedBy: *actor})
	if err != nil {
		return fmt.Errorf("bridge close failed: %w", err)
	}
	return json.NewEncoder(out).Encode(map[string]any{"invoice": invoice, "created": created, "review_sha256": *digest})
}
