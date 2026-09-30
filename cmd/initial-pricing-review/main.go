// initial-pricing-review computes the exact digest of a complete first
// non-OTA TWD card. It is an offline technical check, not publication.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("initial-pricing-review", flag.ContinueOnError)
	candidate := flags.String("candidate", "", "JSON file containing the complete non-OTA rates array")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *candidate == "" {
		return errors.New("--candidate is required")
	}
	file, err := os.Open(*candidate)
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
	digest, err := billing.ReviewInitialCandidateRates(rates)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(map[string]any{
		"status": "technical_review_only", "rate_count": len(rates), "rate_set_sha256": digest,
	})
}
