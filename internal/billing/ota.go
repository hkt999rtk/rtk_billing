package billing

import "time"

// OTA facts are scoped to a Product. Prices remain draft until an operator
// includes all required rates in an activated pricing version.
const (
	ServiceOTA                       = "ota"
	MetricOTADeviceTask              = "device_task"
	MetricOTASuccessfulDownloadGiB   = "successful_download_gib"
	MetricOTAArtifactStorageGiBMonth = "artifact_storage_gib_month"
	MetricOTAArtifactWrite           = "artifact_write"
	UnitOTADeviceTask                = "tasks"
	UnitOTAGiB                       = "GiB"
	UnitOTAGiBMonth                  = "GiB-month"
	UnitOTAArtifactWrite             = "requests"
	otaFractionalQuantityScale       = 9
)

// ValidOTAUsageFact validates the additional product, unit and precision
// contract for a trusted OTA source fact. Non-OTA facts use their own contracts.
func ValidOTAUsageFact(fact UsageFact) bool {
	if fact.ServiceCode != ServiceOTA {
		return true
	}
	if fact.ProductID == "" || fact.Quantity < 0 {
		return false
	}
	switch fact.MetricCode {
	case MetricOTADeviceTask:
		return fact.Unit == UnitOTADeviceTask && fact.QuantityScale == 0 && fact.Quantity == 1
	case MetricOTASuccessfulDownloadGiB:
		return fact.Unit == UnitOTAGiB && fact.QuantityScale == otaFractionalQuantityScale && fact.Quantity > 0
	case MetricOTAArtifactStorageGiBMonth:
		start := fact.WindowStart.UTC()
		end := fact.WindowEnd.UTC()
		monthStart := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
		return fact.Unit == UnitOTAGiBMonth && fact.QuantityScale == otaFractionalQuantityScale &&
			start.Equal(monthStart) && end.Equal(monthStart.AddDate(0, 1, 0))
	case MetricOTAArtifactWrite:
		return fact.Unit == UnitOTAArtifactWrite && fact.QuantityScale == 0 && fact.Quantity == 1
	default:
		return false
	}
}

// ProposedOTARates returns unactivated TWD planning rates before tax. The
// Billing store never installs or activates rates from this helper by itself.
func ProposedOTARates() []PricingRate {
	return []PricingRate{
		{ServiceCode: ServiceOTA, MetricCode: MetricOTADeviceTask, Description: "Firmware OTA device tasks", Unit: UnitOTADeviceTask, UnitPriceMinor: 96, UnitPriceScale: 3, RoundingMode: RoundingHalfUp},
		{ServiceCode: ServiceOTA, MetricCode: MetricOTASuccessfulDownloadGiB, Description: "Verified firmware downloads", Unit: UnitOTAGiB, UnitPriceMinor: 96, UnitPriceScale: 2, RoundingMode: RoundingHalfUp},
		{ServiceCode: ServiceOTA, MetricCode: MetricOTAArtifactStorageGiBMonth, Description: "Firmware artifact storage", Unit: UnitOTAGiBMonth, UnitPriceMinor: 96, UnitPriceScale: 2, RoundingMode: RoundingHalfUp},
		{ServiceCode: ServiceOTA, MetricCode: MetricOTAArtifactWrite, Description: "Firmware artifact writes", Unit: UnitOTAArtifactWrite, UnitPriceMinor: 144, UnitPriceScale: 6, RoundingMode: RoundingHalfUp},
	}
}

// ProposedOTATaskRate preserves the original draft helper for callers that
// need to describe only the task item.
func ProposedOTATaskRate() PricingRate { return ProposedOTARates()[0] }
