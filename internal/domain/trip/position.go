package trip

import (
	"time"

	"github.com/google/uuid"
)

type Position struct {
	ID          int64
	TripID      uuid.UUID
	Coordinates Coordinates
	RecordedAt  time.Time
}

func (t Trip) CanRecordPosition() error {
	if t.Status != StatusActive {
		return ErrTripCompleted
	}
	return nil
}

func (p Position) Validate(now time.Time) error {
	if err := p.Coordinates.Validate("coordinates"); err != nil {
		return err
	}
	if p.RecordedAt.IsZero() {
		return &ValidationError{Field: "recorded_at", Reason: "is required"}
	}
	if p.RecordedAt.After(now.Add(time.Minute)) {
		return &ValidationError{Field: "recorded_at", Reason: "must not be more than one minute in the future"}
	}
	return nil
}

// StaleTrip carries the exact SQL ordering key, reused by keyset pagination.
type StaleTrip struct {
	ID         uuid.UUID
	DriverID   uuid.UUID
	PositionAt time.Time
}
