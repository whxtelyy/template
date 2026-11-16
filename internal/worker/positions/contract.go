package positions

import (
	"context"
	"time"

	"github.com/google/uuid"

	domain "github.com/whxtelyy/template/internal/domain/trip"
)

//go:generate go tool mockgen -source=contract.go -destination=mocks_test.go -package=positions
type repository interface {
	StaleTrips(context.Context, *domain.StaleTrip, time.Duration, int) ([]domain.StaleTrip, error)
}
type gateway interface {
	RequestPosition(context.Context, uuid.UUID, uuid.UUID) error
}
type ticker interface {
	Ticks() <-chan time.Time
	Stop()
}

// Clock is shared by composition only; consumers accept their own narrow interfaces.
type Clock struct{}

func (Clock) Now() time.Time { return time.Now() }

type Ticker struct{ *time.Ticker }

func (t Ticker) Ticks() <-chan time.Time { return t.C }
