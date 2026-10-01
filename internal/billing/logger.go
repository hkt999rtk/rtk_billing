package billing

const (
	ServiceLogger        = "logger"
	MetricLoggerIngest   = "ingest_gib"
	MetricLoggerRetained = "retained_gib_month"
)

// LoggerPricingState recognizes the complete canonical two-meter card. A
// legacy retention alias cannot silently price immutable canonical facts.
func LoggerPricingState(rates []PricingRate) (enabled, complete bool) {
	seen := map[string]bool{}
	complete = true
	for _, rate := range rates {
		if rate.ServiceCode != ServiceLogger {
			continue
		}
		enabled = true
		unit := ""
		switch rate.MetricCode {
		case MetricLoggerIngest:
			unit = "GiB"
		case MetricLoggerRetained:
			unit = "GiB-month"
		default:
			complete = false
		}
		if seen[rate.MetricCode] || unit == "" || rate.Unit != unit || rate.QuantityScale == nil || *rate.QuantityScale != 9 {
			complete = false
		}
		seen[rate.MetricCode] = true
	}
	return enabled, complete && (!enabled || len(seen) == 2 && seen[MetricLoggerIngest] && seen[MetricLoggerRetained])
}
