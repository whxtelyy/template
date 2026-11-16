package positions

import (
	"context"
	"time"

	"github.com/google/uuid"

	domain "github.com/whxtelyy/template/internal/domain/trip"
)

//go:generate go tool mockgen -source=contract.go -destination=mocks_test.go -package=positions
type repository interface {
	Get(context.Context, uuid.UUID) (domain.Trip, error)
	SavePosition(context.Context, domain.Position) (domain.Position, error)
	ListPositions(context.Context, uuid.UUID) ([]domain.Position, error)
}
type txManager interface {
	Do(context.Context, func(context.Context) error) error
}
type (
	clock       interface{ Now() time.Time }
	invalidator interface{ Cancel(uuid.UUID) }
	recorder    interface{ RecordSaved(context.Context) }
)
