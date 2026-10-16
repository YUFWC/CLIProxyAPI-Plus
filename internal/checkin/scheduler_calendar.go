package checkin

import (
	"errors"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// resolveWall rejects both folds and gaps instead of accepting time.Date's normalization.
func resolveWall(day string, minutes int, location *time.Location) (time.Time, error) {
	date, err := time.Parse("2006-01-02", day)
	if err != nil || minutes < 0 || minutes > 1440 {
		return time.Time{}, errors.New("invalid_calendar_time")
	}
	wall := date.Add(time.Duration(minutes) * time.Minute)
	offsets := make(map[int]struct{})
	// Examine both sides of a transition, including historical non-hour offsets.
	for delta := -48 * time.Hour; delta <= 48*time.Hour; delta += 30 * time.Minute {
		_, offset := wall.Add(delta).In(location).Zone()
		offsets[offset] = struct{}{}
	}
	var result time.Time
	matches := 0
	for offset := range offsets {
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(location)
		if local.Format("2006-01-02 15:04:05") == wall.Format("2006-01-02 15:04:05") {
			result = candidate.In(location)
			matches++
		}
	}
	if matches == 0 {
		return time.Time{}, errors.New("nonexistent_local_time")
	}
	if matches != 1 {
		return time.Time{}, errors.New("ambiguous_local_time")
	}
	return result, nil
}

func windowMinutes(value string, end bool) (int, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, errors.New("invalid_window")
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, errors.New("invalid_window")
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 || hour < 0 || hour > 24 || (hour == 24 && (!end || minute != 0)) {
		return 0, errors.New("invalid_window")
	}
	return hour*60 + minute, nil
}

func calendarWindow(day string, cfg config.CheckinProviderConfig, location *time.Location) (time.Time, time.Time, error) {
	startMinutes, err := windowMinutes(cfg.Window.Start, false)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	endMinutes, err := windowMinutes(cfg.Window.End, true)
	if err != nil || startMinutes >= endMinutes {
		return time.Time{}, time.Time{}, errors.New("invalid_window")
	}
	start, err := resolveWall(day, startMinutes, location)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := resolveWall(day, endMinutes, location)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, errors.New("invalid_calendar_window")
	}
	return start, end, nil
}

func adjacentDate(day string, delta int) string {
	date, _ := time.Parse("2006-01-02", day)
	return date.AddDate(0, 0, delta).Format("2006-01-02")
}
