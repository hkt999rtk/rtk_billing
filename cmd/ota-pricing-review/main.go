// ota-pricing-review reads one complete candidate card and compares it with
// the currently selected TWD card. It never writes to Billing.
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

	"github.com/hkt999rtk/rtk_billing/internal/billing"
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
	flags := flag.NewFlagSet("ota-pricing-review", flag.ContinueOnError)
	baseID := flags.String("base-version", "", "current TWD pricing version ID")
	effective := flags.String("effective-from", "", "future UTC month start, RFC3339")
	candidateFile := flags.String("candidate", "", "JSON file containing the complete rates array")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*baseID) == "" || strings.TrimSpace(*candidateFile) == "" {
		return errors.New("--base-version, --effective-from and --candidate are required")
	}
	cutover, err := time.Parse(time.RFC3339, *effective)
	if err != nil {
		return errors.New("--effective-from must be RFC3339")
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	file, err := os.Open(*candidateFile)
	if err != nil {
		return fmt.Errorf("open candidate: %w", err)
	}
	defer file.Close()
	var rates []billing.PricingRate
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rates); err != nil {
		return fmt.Errorf("parse candidate: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("candidate must contain exactly one JSON rates array")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		return errors.New("database connection failed")
	}
	defer db.Close()
	review, err := billingstore.New(db).ReviewOTACandidate(ctx, billingstore.ReviewOTACandidateInput{
		BaseVersionID: strings.TrimSpace(*baseID), EffectiveFrom: cutover, Rates: rates,
	})
	if err != nil {
		return fmt.Errorf("technical OTA card review failed: %w", err)
	}
	return json.NewEncoder(output).Encode(struct {
		Status string `json:"status"`
		billingstore.OTACandidateSnapshot
	}{Status: "technical_review_only", OTACandidateSnapshot: review})
}
