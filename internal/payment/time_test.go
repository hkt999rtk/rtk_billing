package payment

import (
	"testing"
	"time"
)

func TestDailyLimitWindowUsesTaipeiCalendarDay(t *testing.T) {
	now := time.Date(2026, 8, 17, 16, 30, 0, 0, time.UTC) // 2026-08-18 00:30 in Taipei.
	start, next := DailyLimitWindow(now)
	if want := time.Date(2026, 8, 17, 16, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Fatalf("start = %s, want %s", start, want)
	}
	if want := time.Date(2026, 8, 18, 16, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
}
