package billingstore

import (
	"context"
	"strconv"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

// verifyOTAFactGrants runs only for an OTA-priced month, after the producer
// and Platform seals match. An aggregate positive storage fact still requires
// per-object evidence and cannot be charged by this event-grant check.
func (s *Store) verifyOTAFactGrants(ctx context.Context, facts []billing.UsageFact) error {
	seen := make(map[string]struct{})
	for _, fact := range facts {
		if fact.ServiceCode != billing.ServiceOTA {
			continue
		}
		if fact.MetricCode == billing.MetricOTAArtifactStorageGiBMonth {
			if fact.Quantity > 0 {
				return ErrIncomplete
			}
			continue
		}
		if fact.OTAGrant == nil || s.otaGrantVerifier == nil {
			return ErrIncomplete
		}
		witness := *fact.OTAGrant
		key := fact.OrganizationID + "/" + fact.ProductID + "/" +
			strconv.FormatInt(witness.ProductServiceRevision, 10) + "/" +
			witness.ServiceGrantSHA256 + "/" + witness.AuthorizedAt.UTC().Format(time.RFC3339Nano)
		if _, ok := seen[key]; ok {
			continue
		}
		if err := s.otaGrantVerifier.VerifyOTAGrant(ctx, fact.OrganizationID, fact.ProductID, witness); err != nil {
			return ErrIncomplete
		}
		seen[key] = struct{}{}
	}
	return nil
}
