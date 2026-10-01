package billingstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

func testLoggerSeal() LoggerPeriodSeal {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	highWater, _ := json.Marshal(LoggerSourceHighWater{SchemaVersion: 1, SourceID: "55555555-5555-4555-8555-555555555555", Ledger: "device_logger_receipts", CoverageFrom: start, Cutoff: end})
	emptySource := sha256.Sum256([]byte("[]"))
	return LoggerPeriodSeal{OrganizationID: "11111111-1111-4111-8111-111111111111", PeriodStart: start, PeriodEnd: end, IssuerKind: LoggerIssuerProducer,
		SealID: "22222222-2222-4222-8222-222222222222", SourceHighWater: highWater, ProductIDs: []string{},
		MetricCounts: map[string]int64{billing.MetricLoggerIngest: 0, billing.MetricLoggerRetained: 0}, FactSetSHA256: hex.EncodeToString(emptySource[:]), SourceSHA256: hex.EncodeToString(emptySource[:]), SealedAt: end.Add(24 * time.Hour)}
}

func TestLoggerSealRejectsUnprovenCoverageAndPartialMetricDeclarations(t *testing.T) {
	valid := testLoggerSeal()
	if _, _, err := canonicalLoggerPeriodSeal(valid); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*LoggerPeriodSeal){
		"wrong issuer":     func(s *LoggerPeriodSeal) { s.IssuerKind = OTAIssuerProducer },
		"partial month":    func(s *LoggerPeriodSeal) { s.PeriodStart = s.PeriodStart.Add(time.Hour) },
		"before grace":     func(s *LoggerPeriodSeal) { s.SealedAt = s.SealedAt.Add(-time.Microsecond) },
		"future":           func(s *LoggerPeriodSeal) { s.SealedAt = time.Now().Add(time.Hour) },
		"missing products": func(s *LoggerPeriodSeal) { s.ProductIDs = nil },
		"missing metric":   func(s *LoggerPeriodSeal) { s.MetricCounts = map[string]int64{billing.MetricLoggerIngest: 0} },
		"wrong count": func(s *LoggerPeriodSeal) {
			s.MetricCounts = map[string]int64{billing.MetricLoggerIngest: 1, billing.MetricLoggerRetained: 0}
		},
		"untyped highwater": func(s *LoggerPeriodSeal) { s.SourceHighWater = json.RawMessage(`{"count":0}`) },
		"missing zero field": func(s *LoggerPeriodSeal) {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(s.SourceHighWater, &fields)
			delete(fields, "pending_count")
			s.SourceHighWater, _ = json.Marshal(fields)
		},
		"null zero field": func(s *LoggerPeriodSeal) {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(s.SourceHighWater, &fields)
			fields["pending_count"] = json.RawMessage("null")
			s.SourceHighWater, _ = json.Marshal(fields)
		},
		"zero source mismatch": func(s *LoggerPeriodSeal) { s.SourceSHA256 = strings.Repeat("b", 64) },
		"unknown highwater": func(s *LoggerPeriodSeal) {
			s.SourceHighWater = append(s.SourceHighWater[:len(s.SourceHighWater)-1], []byte(`,"invented":true}`)...)
		},
		"uncovered month": func(s *LoggerPeriodSeal) {
			var h LoggerSourceHighWater
			_ = json.Unmarshal(s.SourceHighWater, &h)
			h.CoverageFrom = s.PeriodStart.Add(time.Hour)
			s.SourceHighWater, _ = json.Marshal(h)
		},
		"pending source": func(s *LoggerPeriodSeal) {
			var h LoggerSourceHighWater
			_ = json.Unmarshal(s.SourceHighWater, &h)
			h.PendingCount = 1
			s.SourceHighWater, _ = json.Marshal(h)
		},
		"wrong cutoff": func(s *LoggerPeriodSeal) {
			var h LoggerSourceHighWater
			_ = json.Unmarshal(s.SourceHighWater, &h)
			h.Cutoff = h.Cutoff.Add(-time.Microsecond)
			s.SourceHighWater, _ = json.Marshal(h)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := testLoggerSeal()
			mutate(&input)
			if _, _, err := canonicalLoggerPeriodSeal(input); err == nil {
				t.Fatal("invalid source seal accepted")
			}
		})
	}
}
