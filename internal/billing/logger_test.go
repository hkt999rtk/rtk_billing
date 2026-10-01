package billing

import "testing"

func TestLoggerPricingRequiresBothCanonicalMeters(t *testing.T) {
	scale := 9
	rates := []PricingRate{{ServiceCode: ServiceLogger, MetricCode: MetricLoggerIngest, Unit: "GiB", QuantityScale: &scale}, {ServiceCode: ServiceLogger, MetricCode: MetricLoggerRetained, Unit: "GiB-month", QuantityScale: &scale}}
	if enabled, complete := LoggerPricingState(rates); !enabled || !complete {
		t.Fatal("complete Logger card rejected")
	}
	for _, invalid := range [][]PricingRate{rates[:1], append(append([]PricingRate{}, rates...), rates[0]), {{ServiceCode: ServiceLogger, MetricCode: "retention_gib_month", Unit: "GiB-month", QuantityScale: &scale}}} {
		if enabled, complete := LoggerPricingState(invalid); !enabled || complete {
			t.Fatal("partial or legacy Logger card accepted")
		}
	}
	if enabled, complete := LoggerPricingState(nil); enabled || !complete {
		t.Fatal("Logger absent state invalid")
	}
}
