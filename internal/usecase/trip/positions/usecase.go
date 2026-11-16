package positions

import (
	"context"

	"github.com/google/uuid"

	domain "github.com/whxtelyy/template/internal/domain/trip"
)

type UseCase struct {
	repo    repository
	tx      txManager
	clock   clock
	pending invalidator
	metrics recorder
}

func New(repo repository, tx txManager, clock clock, pending invalidator, metrics recorder) *UseCase {
	return &UseCase{repo: repo, tx: tx, clock: clock, pending: pending, metrics: metrics}
}

func (u *UseCase) Save(ctx context.Context, p domain.Position) (domain.Position, error) {
	if err := p.Validate(u.clock.Now()); err != nil {
		return domain.Position{}, err
	}
	var saved domain.Position
	err := u.tx.Do(ctx, func(ctx context.Context) error {
		trip, err := u.repo.Get(ctx, p.TripID)
		if err != nil {
			return err
		}
		if err := trip.CanRecordPosition(); err != nil {
			return err
		}
		saved, err = u.repo.SavePosition(ctx, p)
		return err
	})
	if err != nil {
		return domain.Position{}, err
	}
	// Effects are deliberately outside the transaction, after a successful COMMIT.
	u.pending.Cancel(p.TripID)
	u.metrics.RecordSaved(ctx)
	return saved, nil
}

func (u *UseCase) List(ctx context.Context, id uuid.UUID) ([]domain.Position, error) {
	if _, err := u.repo.Get(ctx, id); err != nil {
		return nil, err
	}
	return u.repo.ListPositions(ctx, id)
}
