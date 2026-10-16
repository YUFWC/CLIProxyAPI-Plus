package checkin

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCalendarRejectsMissingAndAmbiguousWallTimes(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resolveWall("2026-03-08", 2*60+30, location); err == nil || err.Error() != "nonexistent_local_time" {
		t.Fatalf("DST gap must be explicit: %v", err)
	}
	if _, err = resolveWall("2026-11-01", 1*60+30, location); err == nil || err.Error() != "ambiguous_local_time" {
		t.Fatalf("DST fold must be explicit: %v", err)
	}
}

func TestCalendarUsesNaturalDaysRatherThanTwentyFourHours(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.CheckinProviderConfig{Window: config.CheckinWindow{Start: "00:00", End: "24:00"}}
	start, end, err := calendarWindow("2026-03-08", cfg, location)
	if err != nil {
		t.Fatal(err)
	}
	if end.Sub(start) != 23*time.Hour || end.In(location).Format("2006-01-02 15:04") != "2026-03-09 00:00" {
		t.Fatalf("spring natural day was treated as 24h: %v to %v", start, end)
	}
	start, end, err = calendarWindow("2026-11-01", cfg, location)
	if err != nil {
		t.Fatal(err)
	}
	if end.Sub(start) != 25*time.Hour {
		t.Fatalf("autumn natural day was treated as 24h: %v", end.Sub(start))
	}
}

func TestCalendarRejectsSkippedCivilDate(t *testing.T) {
	location, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resolveWall("2011-12-30", 0, location); err == nil {
		t.Fatal("a skipped civil date must not normalize into another day")
	}
}
