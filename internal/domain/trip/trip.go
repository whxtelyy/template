package trip

import (
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
)

const (
	StatusActive    = "active"
	StatusCompleted = "completed"
)

type Coordinates struct {
	Latitude  float64
	Longitude float64
}

type Trip struct {
	ID             uuid.UUID
	UserID         uuid.UUID
	DriverID       uuid.UUID
	StartPoint     Coordinates
	EndPoint       Coordinates
	Price          int64
	Status         string
	StartedAt      time.Time
	FinishedAt     *time.Time
	LastPositionAt *time.Time
}

type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Reason) }

func (c Coordinates) Validate(field string) error {
	if math.IsNaN(c.Latitude) || math.IsInf(c.Latitude, 0) || c.Latitude < -90 || c.Latitude > 90 {
		return &ValidationError{Field: field + ".latitude", Reason: "must be between -90 and 90"}
	}
	if math.IsNaN(c.Longitude) || math.IsInf(c.Longitude, 0) || c.Longitude < -180 || c.Longitude > 180 {
		return &ValidationError{Field: field + ".longitude", Reason: "must be between -180 and 180"}
	}
	return nil
}

func (t Trip) CanFinish() error {
	if t.Status != StatusActive {
		return ErrTripCompleted
	}
	return nil
}

func (t *Trip) Finish(at time.Time) error {
	if err := t.CanFinish(); err != nil {
		return err
	}
	t.Status = StatusCompleted
	t.FinishedAt = &at
	return nil
}
