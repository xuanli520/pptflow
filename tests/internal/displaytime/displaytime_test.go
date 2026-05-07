package displaytime_test

import (
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/displaytime"
)

func TestFormatMinuteConvertsUTCToShanghai(t *testing.T) {
	got := displaytime.FormatMinute("2026-05-07T05:36:18Z")
	if got != "2026-05-07 13:36" {
		t.Fatalf("FormatMinute = %q", got)
	}
}

func TestFormatSecondConvertsUTCToShanghai(t *testing.T) {
	got := displaytime.FormatSecond("2026-05-07T05:36:18Z")
	if got != "2026-05-07 13:36:18 CST" {
		t.Fatalf("FormatSecond = %q", got)
	}
}

func TestFormatMinuteInvalidInputIsWidthSafe(t *testing.T) {
	got := displaytime.FormatMinute("not-a-valid-rfc3339-value")
	if len(got) > len("2006-01-02 15:04") {
		t.Fatalf("fallback width = %d for %q", len(got), got)
	}
}

func TestRunIDUsesShanghaiTimeAndMicrosecondSuffix(t *testing.T) {
	start := time.Date(2026, 5, 7, 5, 36, 18, 301874000, time.UTC)
	got := displaytime.RunID(start)
	if got != "run-20260507-133618-301874" {
		t.Fatalf("RunID = %q", got)
	}
	other := displaytime.RunID(start.Add(1 * time.Microsecond))
	if other == got || other != "run-20260507-133618-301875" {
		t.Fatalf("RunID collision or wrong suffix: %q vs %q", got, other)
	}
}
