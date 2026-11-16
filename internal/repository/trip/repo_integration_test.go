//go:build integration

package trip

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	domain "github.com/whxtelyy/template/internal/domain/trip"
	"github.com/whxtelyy/template/internal/infra/postgres/transaction"
)

func TestCreateRollsBackAfterInvalidHistoryInsert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool := testPool(t)
	repo := NewTripRepository(pool, 5*time.Second)
	tx := transaction.NewManager(pool, pgx.ReadCommitted, 5*time.Second, slog.Default())
	id := uuid.New()
	trip := domain.Trip{ID: id, UserID: uuid.New(), DriverID: uuid.New(), Status: domain.StatusActive}
	defer func() { _, _ = pool.Exec(context.Background(), "DELETE FROM trips WHERE id=$1", id) }()
	err := tx.Do(ctx, func(txCtx context.Context) error {
		if _, err := repo.Create(txCtx, trip); err != nil {
			return err
		}
		return repo.AddStatusHistory(txCtx, id, nil, "invalid_status", "force constraint failure")
	})
	if err == nil {
		t.Fatal("expected history constraint failure")
	}
	_, err = repo.Get(ctx, id)
	if !errors.Is(err, domain.ErrTripNotFound) {
		t.Fatalf("trip remained after rollback: %v", err)
	}
}
