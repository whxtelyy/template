package positions

import (
	"context"

	"github.com/google/uuid"

	domain "github.com/whxtelyy/template/internal/domain/trip"
)

//go:generate go tool mockgen -source=contract.go -destination=mocks_test.go -package=positions
type usecase interface {
	Save(context.Context, domain.Position) (domain.Position, error)
	List(context.Context, uuid.UUID) ([]domain.Position, error)
}
