package billingstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hkt999rtk/rtk_billing/internal/billing"
)

func TestPutOTAUsageFactRejectsSubmicrosecondWindowBeforeNormalization(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 1, time.UTC)
	fact := billing.UsageFact{
		UsageID: "ota-test", OrganizationID: "11111111-1111-4111-8111-111111111111",
		ProductID:   "22222222-2222-4222-8222-222222222222",
		ServiceCode: billing.ServiceOTA, MetricCode: billing.MetricOTADeviceTask,
		Quantity: 1, Unit: billing.UnitOTADeviceTask,
		WindowStart: start, WindowEnd: start.Add(time.Minute),
		Source: "test", SourceSHA256: strings.Repeat("a", 64),
	}
	var store Store
	if _, _, err := store.PutUsageFact(context.Background(), fact); !errors.Is(err, ErrConflict) {
		t.Fatalf("submicrosecond OTA window: got %v, want conflict before database access", err)
	}
}
