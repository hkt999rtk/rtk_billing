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
	BaseVersionID           string        `json:"base_version_id"`
	RateSetSHA256           string        `json:"rate_set_sha256"`
	AddedOTARates           []PricingRate `json:"added_ota_rates"`
	AddedLoggerRates        []PricingRate `json:"added_logger_rates,omitempty"`
	LoggerApprovalReference string        `json:"logger_approval_reference,omitempty"`
	LoggerRateSetSHA256     string        `json:"logger_rate_set_sha256,omitempty"`
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
	return ReviewOTACandidateRatesWithLogger(baseVersionID, base, candidate, "")
}

// ReviewOTACandidateRatesWithLogger allows only the explicitly approved
// canonical Logger pair as additional rates. Its separate digest binds all
// Logger terms for the subsequent immutable draft and publication records.
// Without Logger additions, the approval reference must be absent.
func ReviewOTACandidateRatesWithLogger(baseVersionID string, base, candidate []PricingRate, loggerApprovalReference string) (OTACardReview, error) {
	loggerAdditions := len(candidate) == len(base)+len(ProposedOTARates())+2
	if strings.TrimSpace(baseVersionID) == "" || len(base) == 0 ||
		(!loggerAdditions && len(candidate) != len(base)+len(ProposedOTARates())) {
		return OTACardReview{}, fmt.Errorf("%w: base identity or full rate count is missing", ErrOTACardReview)
	}
	if loggerAdditions != (loggerApprovalReference != "") ||
		strings.TrimSpace(loggerApprovalReference) != loggerApprovalReference {
		return OTACardReview{}, fmt.Errorf("%w: Logger pair requires an explicit approval reference", ErrOTACardReview)
	}
	baseByIdentity := make(map[string]PricingRate, len(base))
	for _, rate := range base {
		if rate.ServiceCode == ServiceOTA {
			return OTACardReview{}, fmt.Errorf("%w: base already contains OTA", ErrOTACardReview)
		}
		if loggerAdditions && rate.ServiceCode == ServiceLogger {
			return OTACardReview{}, fmt.Errorf("%w: Logger pair cannot replace or supplement existing Logger rates", ErrOTACardReview)
		}
		if strings.TrimSpace(rate.ServiceCode) != rate.ServiceCode || rate.ServiceCode == "" ||
			strings.TrimSpace(rate.MetricCode) != rate.MetricCode || rate.MetricCode == "" ||
			strings.TrimSpace(rate.Unit) != rate.Unit || rate.Unit == "" {
			return OTACardReview{}, fmt.Errorf("%w: invalid base rate identity", ErrOTACardReview)
		}
		key := rate.ServiceCode + "\x00" + rate.MetricCode + "\x00" + rate.Unit
		if _, duplicate := baseByIdentity[key]; duplicate {
			return OTACardReview{}, fmt.Errorf("%w: duplicate base rate %s/%s", ErrOTACardReview, rate.ServiceCode, rate.MetricCode)
		}
		baseByIdentity[key] = rate
	}
	if enabled, complete := OTAPricingState(candidate); !enabled || !complete {
		return OTACardReview{}, fmt.Errorf("%w: OTA meter set differs from approved prices", ErrOTACardReview)
	}
	ordered := make([]canonicalRate, 0, len(candidate))
	added := make([]PricingRate, 0, len(ProposedOTARates()))
	logger := make([]PricingRate, 0, 2)
	loggerCanonical := make([]canonicalRate, 0, 2)
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
		} else if loggerAdditions && rate.ServiceCode == ServiceLogger {
			if !approvedLoggerAddition(canonical) {
				return OTACardReview{}, fmt.Errorf("%w: Logger meter differs from approved terms", ErrOTACardReview)
			}
			logger = append(logger, rate)
			loggerCanonical = append(loggerCanonical, canonical)
		} else if prior, exists := baseByIdentity[key]; !exists || !matchesBaseRate(prior, canonical) {
			return OTACardReview{}, fmt.Errorf("%w: non-OTA rate %s/%s was added or changed", ErrOTACardReview, rate.ServiceCode, rate.MetricCode)
		}
		ordered = append(ordered, canonical)
	}
	if len(added) != len(ProposedOTARates()) || len(seen) != len(baseByIdentity)+len(added)+len(logger) ||
		loggerAdditions && len(logger) != 2 {
		return OTACardReview{}, fmt.Errorf("%w: candidate does not preserve the complete base", ErrOTACardReview)
	}
	sort.Slice(ordered, func(i, j int) bool { return rateIdentity(ordered[i]) < rateIdentity(ordered[j]) })
	sort.Slice(added, func(i, j int) bool { return added[i].MetricCode < added[j].MetricCode })
	raw, err := json.Marshal(ordered)
	if err != nil {
		return OTACardReview{}, err
	}
	digest := sha256.Sum256(raw)
	review := OTACardReview{BaseVersionID: baseVersionID, RateSetSHA256: hex.EncodeToString(digest[:]), AddedOTARates: added}
	if loggerAdditions {
		sort.Slice(loggerCanonical, func(i, j int) bool { return rateIdentity(loggerCanonical[i]) < rateIdentity(loggerCanonical[j]) })
		sort.Slice(logger, func(i, j int) bool { return logger[i].MetricCode < logger[j].MetricCode })
		raw, err := json.Marshal(loggerCanonical)
		if err != nil {
			return OTACardReview{}, err
		}
		digest := sha256.Sum256(raw)
		review.AddedLoggerRates = logger
		review.LoggerApprovalReference = loggerApprovalReference
		review.LoggerRateSetSHA256 = hex.EncodeToString(digest[:])
	}
	return review, nil
}

func approvedLoggerAddition(rate canonicalRate) bool {
	if rate.ServiceCode != ServiceLogger || rate.QuantityScale != 9 || rate.UnitPriceScale != 2 ||
		rate.RoundingMode != RoundingHalfUp || rate.TaxCategory != "standard" || rate.TaxRateBasisPoints != 0 {
		return false
	}
	switch rate.MetricCode {
	case MetricLoggerIngest:
		return rate.Unit == "GiB" && rate.UnitPriceMinor == 2880
	case MetricLoggerRetained:
		return rate.Unit == "GiB-month" && rate.UnitPriceMinor == 131
	default:
		return false
	}
}

// Legacy cards predate quantity_scale and tax_category. A reviewed candidate
// may fill only those missing fields; every previously recorded value and all
// monetary terms must stay identical. The completed candidate is digest-bound.
func matchesBaseRate(base PricingRate, candidate canonicalRate) bool {
	return base.ServiceCode == candidate.ServiceCode && base.MetricCode == candidate.MetricCode &&
		base.Unit == candidate.Unit && base.Description == candidate.Description &&
		base.UnitPriceMinor == candidate.UnitPriceMinor && base.UnitPriceScale == candidate.UnitPriceScale &&
		base.RoundingMode == candidate.RoundingMode && base.TaxRateBasisPoints == candidate.TaxRateBasisPoints &&
		(base.QuantityScale == nil || *base.QuantityScale == candidate.QuantityScale) &&
		(base.TaxCategory == nil || *base.TaxCategory == candidate.TaxCategory)
}
