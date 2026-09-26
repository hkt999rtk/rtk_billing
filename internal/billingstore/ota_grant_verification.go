package billingstore

import (
	"context"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

// VerifyOTAFactGrants runs only for an OTA-priced month, after the producer
// and Platform seals match. Storage requires one independently grant-checked
// object fact and exact byte-time contribution per physical object.
func (s *Store) VerifyOTAFactGrants(ctx context.Context, facts []billing.UsageFact) error {
	seen := make(map[string]struct{})
	seenObjects := make(map[string]struct{})
	type storageTotal struct {
		bytes    *big.Int
		quantity int64
		month    time.Duration
	}
	storage := make(map[string]*storageTotal)
	for _, fact := range facts {
		if fact.ServiceCode != billing.ServiceOTA {
			continue
		}
		if fact.MetricCode == billing.MetricOTAArtifactStorageGiBMonth {
			object := fact.OTAStorageObject
			if object == nil && fact.Quantity == 0 {
				continue // Legacy preactivation zero aggregate cannot add a charge.
			}
			if object == nil || fact.OTAGrant == nil || !otaUTCMonth(fact.WindowStart, fact.WindowEnd) ||
				len(object.ObjectSHA256) != 64 || len(object.ByteMicroseconds) > 40 {
				return ErrIncomplete
			}
			value, ok := new(big.Int).SetString(object.ByteMicroseconds, 10)
			if !ok || value.Sign() < 0 || value.String() != object.ByteMicroseconds {
				return ErrIncomplete
			}
			objectKey := fact.OrganizationID + "/" + fact.WindowStart.UTC().Format(time.RFC3339Nano) + "/" + object.ObjectSHA256
			if _, duplicate := seenObjects[objectKey]; duplicate {
				return ErrIncomplete
			}
			seenObjects[objectKey] = struct{}{}
			key := fact.OrganizationID + "/" + fact.ProductID + "/" + fact.WindowStart.UTC().Format(time.RFC3339Nano)
			group := storage[key]
			if group == nil {
				group = &storageTotal{bytes: new(big.Int), month: fact.WindowEnd.Sub(fact.WindowStart)}
				storage[key] = group
			}
			if fact.Quantity < 0 || fact.Quantity > math.MaxInt64-group.quantity {
				return ErrIncomplete
			}
			group.bytes.Add(group.bytes, value)
			group.quantity += fact.Quantity
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
	for _, group := range storage {
		denominator := new(big.Int).Mul(big.NewInt(group.month.Microseconds()), big.NewInt(1<<30))
		if denominator.Sign() <= 0 {
			return ErrIncomplete
		}
		numerator := new(big.Int).Mul(group.bytes, big.NewInt(1_000_000_000))
		numerator.Add(numerator, new(big.Int).Rsh(denominator, 1))
		numerator.Div(numerator, denominator)
		if !numerator.IsInt64() || numerator.Int64() != group.quantity {
			return ErrIncomplete
		}
	}
	return nil
}
