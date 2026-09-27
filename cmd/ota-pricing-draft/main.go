// ota-pricing-draft creates a complete, unpublished OTA price-card draft from
// the currently effective TWD card. Publication requires separate reviewers.
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
	flags := flag.NewFlagSet("ota-pricing-draft", flag.ContinueOnError)
	baseID := flags.String("base-version", "", "currently effective TWD pricing version ID")
	effective := flags.String("effective-from", "", "future UTC month start, RFC3339")
	createdBy := flags.String("created-by", "", "operator identity recorded on the draft")
	candidateFile := flags.String("candidate", "", "reviewed complete rate-card JSON, required for legacy base metadata")
	reviewedDigest := flags.String("review-sha256", "", "rate-set SHA-256 from the read-only review")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*baseID) == "" || strings.TrimSpace(*createdBy) == "" {
		return errors.New("--base-version, --effective-from and --created-by are required")
	}
	if (*candidateFile == "") != (*reviewedDigest == "") {
		return errors.New("--candidate and --review-sha256 must be supplied together")
	}
	cutover, err := time.Parse(time.RFC3339, *effective)
	if err != nil {
		return errors.New("--effective-from must be RFC3339")
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	var rates []billing.PricingRate
	if *candidateFile != "" {
		file, err := os.Open(*candidateFile)
		if err != nil {
			return fmt.Errorf("open candidate: %w", err)
		}
		defer file.Close()
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&rates); err != nil {
			return fmt.Errorf("parse candidate: %w", err)
		}
		if decoder.Decode(new(any)) != io.EOF {
			return errors.New("candidate must contain exactly one JSON rates array")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, databaseURL)
	if err != nil {
		return errors.New("database connection failed")
	}
	defer db.Close()
	draft, err := billingstore.New(db).CreateOTAPricingDraft(ctx, billingstore.CreateOTAPricingDraftInput{
		BaseVersionID: strings.TrimSpace(*baseID), EffectiveFrom: cutover, CreatedBy: strings.TrimSpace(*createdBy),
		CandidateRates: rates, ReviewedRateSetSHA256: *reviewedDigest,
	})
	if err != nil {
		return fmt.Errorf("OTA draft creation failed: %w", err)
	}
	return json.NewEncoder(output).Encode(struct {
		Status string `json:"status"`
		billingstore.OTAPricingDraft
	}{Status: "draft_not_effective", OTAPricingDraft: draft})
}
