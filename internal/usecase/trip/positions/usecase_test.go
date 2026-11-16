package positions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	domain "github.com/whxtelyy/template/internal/domain/trip"
)

func TestSaveCommitEffects(t *testing.T) {
	for _, tc := range []struct {
		name               string
		saveErr, commitErr error
	}{
		{name: "committed"}, {name: "insert failed", saveErr: errors.New("insert")}, {name: "commit failed", commitErr: errors.New("commit")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			repo, tx, clock, pending, metrics := NewMockrepository(ctrl), NewMocktxManager(ctrl), NewMockclock(ctrl), NewMockinvalidator(ctrl), NewMockrecorder(ctrl)
			ctx := context.Background()
			now := time.Now()
			p := domain.Position{TripID: uuid.New(), RecordedAt: now}
			saved := p
			saved.ID = 42
			clock.EXPECT().Now().Return(now)
			gomock.InOrder(
				tx.EXPECT().Do(ctx, gomock.Any()).DoAndReturn(func(ctx context.Context, fn func(context.Context) error) error {
					if err := fn(ctx); err != nil {
						return err
					}
					return tc.commitErr
				}),
				repo.EXPECT().Get(ctx, p.TripID).Return(domain.Trip{Status: domain.StatusActive}, nil),
				repo.EXPECT().SavePosition(ctx, p).Return(saved, tc.saveErr),
			)
			if tc.saveErr == nil && tc.commitErr == nil {
				gomock.InOrder(pending.EXPECT().Cancel(p.TripID), metrics.EXPECT().RecordSaved(ctx))
			}
			got, err := New(repo, tx, clock, pending, metrics).Save(ctx, p)
			want := tc.saveErr
			if want == nil {
				want = tc.commitErr
			}
			require.ErrorIs(t, err, want)
			if want == nil {
				require.Equal(t, saved, got)
			}
		})
	}
}

func TestSaveInvalidAndList(t *testing.T) {
	ctrl := gomock.NewController(t)
	repo := NewMockrepository(ctrl)
	clock := NewMockclock(ctrl)
	uc := New(repo, NewMocktxManager(ctrl), clock, NewMockinvalidator(ctrl), NewMockrecorder(ctrl))
	now := time.Now()
	clock.EXPECT().Now().Return(now)
	_, err := uc.Save(context.Background(), domain.Position{RecordedAt: now.Add(2 * time.Minute)})
	require.Error(t, err)
	id := uuid.New()
	repo.EXPECT().Get(gomock.Any(), id).Return(domain.Trip{}, domain.ErrTripNotFound)
	_, err = uc.List(context.Background(), id)
	require.ErrorIs(t, err, domain.ErrTripNotFound)
	repo.EXPECT().Get(gomock.Any(), id).Return(domain.Trip{ID: id}, nil)
	repo.EXPECT().ListPositions(gomock.Any(), id).Return([]domain.Position{}, nil)
	got, err := uc.List(context.Background(), id)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSaveRejectsMissingOrCompletedTrip(t *testing.T) {
	for _, tc := range []struct {
		name         string
		trip         domain.Trip
		getErr, want error
	}{
		{name: "missing", getErr: domain.ErrTripNotFound, want: domain.ErrTripNotFound},
		{name: "completed", trip: domain.Trip{Status: domain.StatusCompleted}, want: domain.ErrTripCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			repo, tx, clock := NewMockrepository(ctrl), NewMocktxManager(ctrl), NewMockclock(ctrl)
			now := time.Now()
			p := domain.Position{TripID: uuid.New(), RecordedAt: now}
			clock.EXPECT().Now().Return(now)
			tx.EXPECT().Do(gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) })
			repo.EXPECT().Get(gomock.Any(), p.TripID).Return(tc.trip, tc.getErr)
			_, err := New(repo, tx, clock, NewMockinvalidator(ctrl), NewMockrecorder(ctrl)).Save(context.Background(), p)
			require.ErrorIs(t, err, tc.want)
		})
	}
}
