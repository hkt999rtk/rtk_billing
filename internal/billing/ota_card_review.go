package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrOTACardReview = errors.New("OTA candidate is not a complete reviewed price card")

// OTACardReview compares the entire proposed rate set with its base. The
// digest binds rate content, not database-generated row IDs. It is technical
// preflight evidence; Finance tax and account-scope approval are separate.
type OTACardReview struct {
	BaseVersionID string        `json:"base_version_id"`
	RateSetSHA256 string        `json:"rate_set_sha256"`
	AddedOTARates []PricingRate `json:"added_ota_rates"`
}

type canonicalRate struct {
	ServiceCode        string       `json:"service_code"`
	MetricCode         string       `json:"metric_code"`
	Unit               string       `json:"unit"`
	Description        string       `json:"description"`
	UnitPriceMinor     int64        `json:"unit_price_minor"`
	UnitPriceScale     int          `json:"unit_price_scale"`
	QuantityScale      int          `json:"quantity_scale"`
	RoundingMode       RoundingMode `json:"rounding_mode"`
	TaxCategory        string       `json:"tax_category"`
	TaxRateBasisPoints int64        `json:"tax_rate_basis_points"`
}

func canonicalPricingRate(rate PricingRate) (canonicalRate, bool) {
	if strings.TrimSpace(rate.ServiceCode) != rate.ServiceCode || rate.ServiceCode == "" ||
		strings.TrimSpace(rate.MetricCode) != rate.MetricCode || rate.MetricCode == "" ||
		strings.TrimSpace(rate.Unit) != rate.Unit || rate.Unit == "" ||
		strings.TrimSpace(rate.Description) != rate.Description || rate.Description == "" ||
		rate.QuantityScale == nil || *rate.QuantityScale < 0 || *rate.QuantityScale > 9 ||
		rate.UnitPriceMinor < 0 || rate.UnitPriceScale < 0 || rate.UnitPriceScale > 9 ||
		(rate.RoundingMode != RoundingHalfUp && rate.RoundingMode != RoundingDown && rate.RoundingMode != RoundingUp) || rate.TaxCategory == nil ||
		strings.TrimSpace(*rate.TaxCategory) != *rate.TaxCategory || *rate.TaxCategory == "" ||
		rate.TaxRateBasisPoints < 0 || rate.TaxRateBasisPoints > 10000 {
		return canonicalRate{}, false
	}
	return canonicalRate{ServiceCode: rate.ServiceCode, MetricCode: rate.MetricCode, Unit: rate.Unit,
		Description: rate.Description, UnitPriceMinor: rate.UnitPriceMinor, UnitPriceScale: rate.UnitPriceScale,
		QuantityScale: *rate.QuantityScale, RoundingMode: rate.RoundingMode,
		TaxCategory: *rate.TaxCategory, TaxRateBasisPoints: rate.TaxRateBasisPoints}, true
}

func rateIdentity(rate canonicalRate) string {
	return rate.ServiceCode + "\x00" + rate.MetricCode + "\x00" + rate.Unit
}

// ReviewOTACandidateRates rejects missing, duplicated or changed non-OTA
// rates and requires the exact four approved OTA meters. A valid review does
// not permit publication: the caller must also prove tax sign-off, account
// applicability, current base version and a future UTC-month cutover.
func ReviewOTACandidateRates(baseVersionID string, base, candidate []PricingRate) (OTACardReview, error) {
	if strings.TrimSpace(baseVersionID) == "" || len(base) == 0 || len(candidate) != len(base)+len(ProposedOTARates()) {
		return OTACardReview{}, fmt.Errorf("%w: base identity or full rate count is missing", ErrOTACardReview)
	}
	baseByIdentity := make(map[string]canonicalRate, len(base))
	for _, rate := range base {
		if rate.ServiceCode == ServiceOTA {
			return OTACardReview{}, fmt.Errorf("%w: base already contains OTA", ErrOTACardReview)
		}
		canonical, ok := canonicalPricingRate(rate)
		if !ok {
			return OTACardReview{}, fmt.Errorf("%w: base rate %s/%s has unresolved metadata", ErrOTACardReview, rate.ServiceCode, rate.MetricCode)
		}
		key := rateIdentity(canonical)
		if _, duplicate := baseByIdentity[key]; duplicate {
			return OTACardReview{}, fmt.Errorf("%w: duplicate base rate %s/%s", ErrOTACardReview, rate.ServiceCode, rate.MetricCode)
		}
		baseByIdentity[key] = canonical
	}
	if enabled, complete := OTAPricingState(candidate); !enabled || !complete {
		return OTACardReview{}, fmt.Errorf("%w: OTA meter set differs from approved prices", ErrOTACardReview)
	}
	ordered := make([]canonicalRate, 0, len(candidate))
	added := make([]PricingRate, 0, len(ProposedOTARates()))
	seen := make(map[string]bool, len(candidate))
	for _, rate := range candidate {
		canonical, ok := canonicalPricingRate(rate)
		if !ok {
			return OTACardReview{}, fmt.Errorf("%w: candidate rate %s/%s has unresolved metadata", ErrOTACardReview, rate.ServiceCode, rate.MetricCode)
		}
		key := rateIdentity(canonical)
		if seen[key] {
			return OTACardReview{}, fmt.Errorf("%w: duplicate candidate rate %s/%s", ErrOTACardReview, rate.ServiceCode, rate.MetricCode)
		}
		seen[key] = true
		if rate.ServiceCode == ServiceOTA {
			added = append(added, rate)
		} else if prior, exists := baseByIdentity[key]; !exists || prior != canonical {
			return OTACardReview{}, fmt.Errorf("%w: non-OTA rate %s/%s was added or changed", ErrOTACardReview, rate.ServiceCode, rate.MetricCode)
		}
		ordered = append(ordered, canonical)
	}
	if len(added) != len(ProposedOTARates()) || len(seen) != len(baseByIdentity)+len(added) {
		return OTACardReview{}, fmt.Errorf("%w: candidate does not preserve the complete base", ErrOTACardReview)
	}
	sort.Slice(ordered, func(i, j int) bool { return rateIdentity(ordered[i]) < rateIdentity(ordered[j]) })
	sort.Slice(added, func(i, j int) bool { return added[i].MetricCode < added[j].MetricCode })
	raw, err := json.Marshal(ordered)
	if err != nil {
		return OTACardReview{}, err
	}
	digest := sha256.Sum256(raw)
	return OTACardReview{BaseVersionID: baseVersionID, RateSetSHA256: hex.EncodeToString(digest[:]), AddedOTARates: added}, nil
}
