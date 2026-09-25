package billing

import (
	"testing"
	"time"
)

func TestProposedOTATaskRateAggregatesDeviceTasks(t *testing.T) {
	rate := ProposedOTATaskRate()
	if rate.ServiceCode != "ota" || rate.MetricCode != "device_task" || rate.Unit != "tasks" {
		t.Fatalf("unexpected OTA billing item: %+v", rate)
	}
	subtotal, _, _, err := PriceUsage(rate, 1000, 0)
	if err != nil || subtotal != 96 {
		t.Fatalf("1000 OTA tasks subtotal = %d, err = %v; want NT$96", subtotal, err)
	}
}

func TestProposedOTARatesProduceFourProductLines(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	rates := ProposedOTARates()
	facts := []UsageFact{
		{UsageID: "task", OrganizationID: "cloud", ProductID: "product", ServiceCode: ServiceOTA, MetricCode: MetricOTADeviceTask, Quantity: 1, Unit: UnitOTADeviceTask, WindowStart: start, WindowEnd: start.Add(time.Minute)},
		{UsageID: "download", OrganizationID: "cloud", ProductID: "product", ServiceCode: ServiceOTA, MetricCode: MetricOTASuccessfulDownloadGiB, Quantity: 100_000_000_000, QuantityScale: 9, Unit: UnitOTAGiB, WindowStart: start, WindowEnd: start.Add(time.Minute)},
		{UsageID: "storage", OrganizationID: "cloud", ProductID: "product", ServiceCode: ServiceOTA, MetricCode: MetricOTAArtifactStorageGiBMonth, Quantity: 100_000_000_000, QuantityScale: 9, Unit: UnitOTAGiBMonth, WindowStart: start, WindowEnd: end},
		{UsageID: "write", OrganizationID: "cloud", ProductID: "product", ServiceCode: ServiceOTA, MetricCode: MetricOTAArtifactWrite, Quantity: 1, Unit: UnitOTAArtifactWrite, WindowStart: start, WindowEnd: start.Add(time.Minute)},
	}
	for _, fact := range facts {
		if !ValidOTAUsageFact(fact) {
			t.Fatalf("valid OTA fact rejected: %+v", fact)
		}
	}
	invoice, err := BuildDraftInvoice(Invoice{OrganizationID: "cloud", PricingVersionID: "proposed", Currency: CurrencyTWD, PeriodStart: start, PeriodEnd: end}, facts, rates)
	if err != nil || len(invoice.Lines) != 4 || invoice.SubtotalMinor != 192 || invoice.TotalMinor != 192 {
		t.Fatalf("four OTA lines: invoice=%+v err=%v", invoice, err)
	}
	writeTotal, _, _, err := PriceUsage(rates[3], 1_000_000, 0)
	if err != nil || writeTotal != 144 {
		t.Fatalf("million write price = %d, err=%v; want NT$144", writeTotal, err)
	}
}

func TestOTAUsageFactRejectsMissingProductAndWrongMetricPrecision(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	valid := UsageFact{ProductID: "product", ServiceCode: ServiceOTA, MetricCode: MetricOTAArtifactStorageGiBMonth,
		Quantity: 500_000_000, QuantityScale: 9, Unit: UnitOTAGiBMonth, WindowStart: start, WindowEnd: start.AddDate(0, 1, 0)}
	if !ValidOTAUsageFact(valid) {
		t.Fatal("valid storage fact rejected")
	}
	zeroStorage := valid
	zeroStorage.Quantity = 0
	if !ValidOTAUsageFact(zeroStorage) {
		t.Fatal("zero-rounded Product/month storage fact rejected")
	}
	zeroDownload := valid
	zeroDownload.MetricCode = MetricOTASuccessfulDownloadGiB
	zeroDownload.Unit = UnitOTAGiB
	zeroDownload.Quantity = 0
	if ValidOTAUsageFact(zeroDownload) {
		t.Fatal("zero-sized download fact accepted")
	}
	for _, change := range []func(*UsageFact){
		func(f *UsageFact) { f.ProductID = "" },
		func(f *UsageFact) { f.Quantity = -1 },
		func(f *UsageFact) { f.MetricCode = "object_read" },
		func(f *UsageFact) { f.Unit = "bytes" },
		func(f *UsageFact) { f.QuantityScale = 0 },
		func(f *UsageFact) { f.WindowEnd = f.WindowEnd.Add(-time.Hour) },
	} {
		invalid := valid
		change(&invalid)
		if ValidOTAUsageFact(invalid) {
			t.Fatalf("invalid OTA fact accepted: %+v", invalid)
		}
	}
}

func TestOTAInstantFactsRequireExactlyOneAlignedUTCMinute(t *testing.T) {
	start := time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC)
	local := time.FixedZone("Taipei", 8*60*60)
	for _, metric := range []struct {
		code, unit string
		quantity   int64
		scale      int
	}{
		{MetricOTADeviceTask, UnitOTADeviceTask, 1, 0},
		{MetricOTASuccessfulDownloadGiB, UnitOTAGiB, 1, otaFractionalQuantityScale},
		{MetricOTAArtifactWrite, UnitOTAArtifactWrite, 1, 0},
	} {
		t.Run(metric.code, func(t *testing.T) {
			valid := UsageFact{ProductID: "product", ServiceCode: ServiceOTA, MetricCode: metric.code,
				Quantity: metric.quantity, QuantityScale: metric.scale, Unit: metric.unit,
				WindowStart: start.In(local), WindowEnd: start.Add(time.Minute).In(local)}
			if !ValidOTAUsageFact(valid) {
				t.Fatal("equivalent non-UTC location rejected")
			}
			for _, test := range []struct {
				name  string
				start time.Time
				end   time.Time
			}{
				{"two minutes", start, start.Add(2 * time.Minute)},
				{"unaligned start", start.Add(time.Second), start.Add(time.Minute + time.Second)},
				{"wrong end", start, start.Add(time.Minute - time.Second)},
				{"submicrosecond start", start.Add(time.Nanosecond), start.Add(time.Minute + time.Nanosecond)},
			} {
				t.Run(test.name, func(t *testing.T) {
					invalid := valid
					invalid.WindowStart, invalid.WindowEnd = test.start, test.end
					if ValidOTAUsageFact(invalid) {
						t.Fatalf("invalid OTA minute accepted: %s to %s", test.start, test.end)
					}
				})
			}
		})
	}
}
