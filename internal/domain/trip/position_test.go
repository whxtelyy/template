package trip

import (
	"math"
	"testing"
	"time"
)

func TestPositionValidation(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		coords  Coordinates
		at      time.Time
		invalid bool
	}{
		{"zero coordinates", Coordinates{}, now, false},
		{"edges", Coordinates{Latitude: 90, Longitude: -180}, now, false},
		{"one minute", Coordinates{}, now.Add(time.Minute), false},
		{"old", Coordinates{}, now.Add(-time.Hour), false},
		{"future", Coordinates{}, now.Add(time.Minute + time.Nanosecond), true},
		{"missing", Coordinates{}, time.Time{}, true},
		{"latitude", Coordinates{Latitude: 91}, now, true},
		{"longitude", Coordinates{Longitude: -181}, now, true},
		{"nan", Coordinates{Latitude: math.NaN()}, now, true},
		{"infinity", Coordinates{Longitude: math.Inf(1)}, now, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (Position{Coordinates: tc.coords, RecordedAt: tc.at}).Validate(now)
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCanRecordPosition(t *testing.T) {
	for _, status := range []string{StatusActive, StatusCompleted} {
		err := (Trip{Status: status}).CanRecordPosition()
		if (err != nil) != (status == StatusCompleted) {
			t.Fatalf("status=%s error=%v", status, err)
		}
	}
}
