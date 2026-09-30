package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

var ErrInitialCardReview = errors.New("initial price card has unresolved rates")

// ReviewInitialCandidateRates binds the complete non-OTA starting card to a
// deterministic digest. A published OTA card must use the separate reviewed
// cutover path after this base exists.
func ReviewInitialCandidateRates(rates []PricingRate) (string, error) {
	if len(rates) == 0 {
		return "", ErrInitialCardReview
	}
	ordered := make([]canonicalRate, 0, len(rates))
	seen := make(map[string]bool, len(rates))
	for _, rate := range rates {
		canonical, ok := canonicalPricingRate(rate)
		if !ok || canonical.ServiceCode == ServiceOTA || canonical.TaxCategory != "standard" || canonical.TaxRateBasisPoints != 0 {
			return "", fmt.Errorf("%w: invalid rate %s/%s", ErrInitialCardReview, rate.ServiceCode, rate.MetricCode)
		}
		key := rateIdentity(canonical)
		if seen[key] {
			return "", fmt.Errorf("%w: duplicate rate %s/%s", ErrInitialCardReview, rate.ServiceCode, rate.MetricCode)
		}
		seen[key] = true
		ordered = append(ordered, canonical)
	}
	sort.Slice(ordered, func(i, j int) bool { return rateIdentity(ordered[i]) < rateIdentity(ordered[j]) })
	raw, err := json.Marshal(ordered)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
