//go:build integration

package trip

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	domain "github.com/whxtelyy/template/internal/domain/trip"
	"github.com/whxtelyy/template/internal/infra/postgres/transaction"
	"github.com/whxtelyy/template/internal/usecase/trip/create"
	"github.com/whxtelyy/template/internal/usecase/trip/finish"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := positionsPool(t)
	return pool
}

func TestIdempotencyReplayAndConflict(t *testing.T) {
	pool := testPool(t)
	repo := NewTripRepository(pool, 5*time.Second)
	uc := create.New(repo, transaction.NewManager(pool, pgx.ReadCommitted, 5*time.Second, slog.Default()), nil)
	key := uuid.New()
	req := create.Request{UserID: uuid.New(), DriverID: uuid.New(), Price: 100, IdempotencyKey: &key, RequestHash: "same"}
	ctx := context.Background()
	first, err := uc.Execute(ctx, req)
	if err != nil || first.Replayed {
		t.Fatalf("first create: %+v, %v", first, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM trip_idempotency_keys WHERE idempotency_key=$1", key)
		_, _ = pool.Exec(context.Background(), "DELETE FROM trips WHERE id=$1", first.Trip.ID)
	})
	second, err := uc.Execute(ctx, req)
	if err != nil || !second.Replayed || second.Trip.ID != first.Trip.ID {
		t.Fatalf("replay: %+v, %v", second, err)
	}
	req.RequestHash = "different"
	_, err = uc.Execute(ctx, req)
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("conflict = %v", err)
	}
	var historyCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM trip_status_history WHERE trip_id=$1", first.Trip.ID).Scan(&historyCount); err != nil || historyCount != 1 {
		t.Fatalf("history count = %d, %v", historyCount, err)
	}
}

func TestConcurrentDriverBusy(t *testing.T) {
	pool := testPool(t)
	repo := NewTripRepository(pool, 5*time.Second)
	uc := create.New(repo, transaction.NewManager(pool, pgx.ReadCommitted, 5*time.Second, slog.Default()), nil)
	driver := uuid.New()
	ctx := context.Background()
	results := make([]create.Result, 2)
	errs := make([]error, 2)
	var group sync.WaitGroup
	for i := range results {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			results[i], errs[i] = uc.Execute(ctx, create.Request{UserID: uuid.New(), DriverID: driver})
		}(i)
	}
	group.Wait()
	var success, busy int
	for i, err := range errs {
		if err == nil {
			success++
			_, _ = pool.Exec(ctx, "DELETE FROM trips WHERE id=$1", results[i].Trip.ID)
		}
		if errors.Is(err, domain.ErrDriverBusy) {
			busy++
		}
	}
	if success != 1 || busy != 1 {
		t.Fatalf("success=%d, driver_busy=%d, errors=%v", success, busy, errs)
	}
}

func TestConcurrentFinishUsesDatabaseTime(t *testing.T) {
	pool := testPool(t)
	repo := NewTripRepository(pool, 5*time.Second)
	tx := transaction.NewManager(pool, pgx.ReadCommitted, 5*time.Second, slog.Default())
	ctx := context.Background()
	created, err := create.New(repo, tx, nil).Execute(ctx, create.Request{UserID: uuid.New(), DriverID: uuid.New(), Price: 100})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Trip.ID
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM trips WHERE id=$1", id) })
	uc := finish.New(repo, tx, nil)
	errs := make([]error, 8)
	var group sync.WaitGroup
	for i := range errs {
		group.Add(1)
		go func(i int) { defer group.Done(); _, errs[i] = uc.Execute(ctx, id) }(i)
	}
	group.Wait()
	var succeeded, completed int
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, domain.ErrTripCompleted):
			completed++
		default:
			t.Fatalf("unexpected finish error: %v", err)
		}
	}
	if succeeded != 1 || completed != 7 {
		t.Fatalf("succeeded=%d completed=%d", succeeded, completed)
	}
	trip, err := repo.Get(ctx, id)
	if err != nil || trip.FinishedAt == nil || trip.Status != domain.StatusCompleted {
		t.Fatalf("trip=%+v err=%v", trip, err)
	}
	var changedAt time.Time
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*), min(changed_at) FROM trip_status_history WHERE trip_id=$1 AND to_status='completed'", id).Scan(&count, &changedAt); err != nil {
		t.Fatal(err)
	}
	if count != 1 || !trip.FinishedAt.Equal(changedAt) {
		t.Fatalf("history count=%d time=%s finished=%s", count, changedAt, trip.FinishedAt)
	}
	_, err = uc.Execute(ctx, uuid.New())
	if !errors.Is(err, domain.ErrTripNotFound) {
		t.Fatalf("missing trip error=%v", err)
	}
}
