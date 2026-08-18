package payment

import "time"

const DailyLimitTimezone = "Asia/Taipei"

var dailyLimitLocation = time.FixedZone(DailyLimitTimezone, 8*60*60)

// DailyLimitWindow returns the UTC instants bounding the calendar day in the
// billing timezone. FixedZone keeps the service independent of host tzdata;
// Taiwan has observed UTC+8 without daylight-saving changes since 1979.
func DailyLimitWindow(now time.Time) (time.Time, time.Time) {
	local := now.In(dailyLimitLocation)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, dailyLimitLocation)
	return start.UTC(), start.AddDate(0, 0, 1).UTC()
}
