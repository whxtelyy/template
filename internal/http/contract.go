package httpapi

import (
	"context"

	"github.com/google/uuid"

	domain "github.com/whxtelyy/template/internal/domain/trip"
	"github.com/whxtelyy/template/internal/usecase/trip/create"
)

//go:generate go tool mockgen -source=contract.go -destination=mocks_test.go -package=httpapi
type createUseCase interface {
	Execute(context.Context, create.Request) (create.Result, error)
}
type getUseCase interface {
	Execute(context.Context, uuid.UUID) (domain.Trip, error)
}
type finishUseCase interface {
	Execute(context.Context, uuid.UUID) (domain.Trip, error)
}
type readiness interface{ Ping(context.Context) error }

type positionsUseCase interface {
	Save(context.Context, domain.Position) (domain.Position, error)
	List(context.Context, uuid.UUID) ([]domain.Position, error)
}
