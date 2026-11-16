//go:build integration

package trip

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	domain "github.com/whxtelyy/template/internal/domain/trip"
	"github.com/whxtelyy/template/internal/infra/postgres/transaction"
)

// Each test owns a fresh schema, including its own Goose version table.
func positionsPool(t *testing.T) (*pgxpool.Pool, *goose.Provider) {
	t.Helper()
	ctx := context.Background()
	url := os.Getenv("DATABASE_URL")
	require.NotEmpty(t, url, "DATABASE_URL is required")
	admin, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := "lab3_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
		if err != nil {
			t.Error(err)
		}
	})
	cfg := admin.Config().Copy()
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["application_name"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() { _ = db.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS("../../../migrations"))
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	return pool, provider
}

func newPositionTrip(t *testing.T, pool *pgxpool.Pool, n int, at time.Time) domain.Trip {
	t.Helper()
	repo := NewTripRepository(pool, time.Second)
	trip, err := repo.Create(context.Background(), domain.Trip{ID: uuid.New(), UserID: uuid.New(), DriverID: uuid.New(), Status: domain.StatusActive, Price: int64(n)})
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), "UPDATE trips SET started_at=$1 WHERE id=$2", at, trip.ID)
	require.NoError(t, err)
	return trip
}

func positionTx(pool *pgxpool.Pool) *transaction.Manager {
	return transaction.NewManager(pool, pgx.ReadCommitted, 2*time.Second, slog.Default())
}

func waitForRowLock(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock'", pool.Config().ConnConfig.RuntimeParams["application_name"]).Scan(&waiting))
		if waiting > 0 {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("concurrent query did not wait for row lock")
		}
	}
}

func savePosition(ctx context.Context, pool *pgxpool.Pool, p domain.Position) (domain.Position, error) {
	var result domain.Position
	err := positionTx(pool).Do(ctx, func(ctx context.Context) error {
		var err error
		result, err = NewTripRepository(pool, time.Second).SavePosition(ctx, p)
		return err
	})
	return result, err
}

func TestPositionMigrationsUpDownUp(t *testing.T) {
	pool, p := positionsPool(t)
	ctx := context.Background()
	for range 5 {
		_, err := p.Down(ctx)
		require.NoError(t, err)
	}
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass(current_schema() || '.trips') IS NOT NULL").Scan(&exists))
	require.False(t, exists)
	_, err := p.Up(ctx)
	require.NoError(t, err)
	var index string
	require.NoError(t, pool.QueryRow(ctx, "SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema() AND indexname='trips_active_position_idx'").Scan(&index))
	require.Contains(t, index, "COALESCE(last_position_at, started_at)")
	require.Contains(t, index, "WHERE (status = 'active'::text)")
}

func TestPositionsAtomicRollbackMonotonicAndSorted(t *testing.T) {
	pool, _ := positionsPool(t)
	repo := NewTripRepository(pool, time.Second)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	trip := newPositionTrip(t, pool, 1, now.Add(-time.Hour))
	p := domain.Position{TripID: trip.ID, RecordedAt: now}
	first, err := savePosition(ctx, pool, p)
	require.NoError(t, err)
	p.RecordedAt = now.Add(-time.Minute)
	old, err := savePosition(ctx, pool, p)
	require.NoError(t, err)
	p.RecordedAt = now
	third, err := savePosition(ctx, pool, p)
	require.NoError(t, err)
	got, err := repo.Get(ctx, trip.ID)
	require.NoError(t, err)
	require.True(t, now.Equal(*got.LastPositionAt))
	points, err := repo.ListPositions(ctx, trip.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{old.ID, first.ID, third.ID}, []int64{points[0].ID, points[1].ID, points[2].ID})
	p.RecordedAt = now.Add(time.Hour)
	p.Coordinates.Latitude = 91 // DB CHECK fails after trip update
	_, err = savePosition(ctx, pool, p)
	require.Error(t, err)
	got, err = repo.Get(ctx, trip.ID)
	require.NoError(t, err)
	require.True(t, now.Equal(*got.LastPositionAt))
	points, err = repo.ListPositions(ctx, trip.ID)
	require.NoError(t, err)
	require.Len(t, points, 3)
	p.Coordinates.Latitude = 0
	p.TripID = uuid.New()
	_, err = savePosition(ctx, pool, p)
	require.ErrorIs(t, err, domain.ErrTripNotFound)
	_, err = repo.Finish(ctx, trip.ID)
	require.NoError(t, err)
	p.TripID = trip.ID
	_, err = savePosition(ctx, pool, p)
	require.ErrorIs(t, err, domain.ErrTripCompleted)
}

func TestConcurrentPositionsAndFinish(t *testing.T) {
	pool, _ := positionsPool(t)
	repo := NewTripRepository(pool, time.Second)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	trip := newPositionTrip(t, pool, 1, now)
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = savePosition(ctx, pool, domain.Position{TripID: trip.ID, RecordedAt: now.Add(time.Duration(i) * time.Second)})
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	got, err := repo.Get(ctx, trip.ID)
	require.NoError(t, err)
	require.True(t, now.Add(time.Second).Equal(*got.LastPositionAt))
	// Deterministically hold Finish's row lock while SavePosition reaches PostgreSQL.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "UPDATE trips SET status='completed',finished_at=now() WHERE id=$1", trip.ID)
	require.NoError(t, err)
	launched, result := make(chan struct{}), make(chan error, 1)
	go func() {
		close(launched)
		_, err := savePosition(ctx, pool, domain.Position{TripID: trip.ID, RecordedAt: now.Add(2 * time.Second)})
		result <- err
	}()
	<-launched
	waitForRowLock(t, pool)
	require.NoError(t, tx.Commit(ctx))
	require.ErrorIs(t, <-result, domain.ErrTripCompleted)
	points, err := repo.ListPositions(ctx, trip.ID)
	require.NoError(t, err)
	require.Len(t, points, 2)
	// Reverse ordering: a position holds the row until COMMIT, then Finish succeeds.
	other := newPositionTrip(t, pool, 2, now)
	positionSaved, releasePosition, positionResult := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		positionResult <- positionTx(pool).Do(ctx, func(ctx context.Context) error {
			if _, err := repo.SavePosition(ctx, domain.Position{TripID: other.ID, RecordedAt: now}); err != nil {
				return err
			}
			close(positionSaved)
			<-releasePosition
			return nil
		})
	}()
	<-positionSaved
	finishResult := make(chan error, 1)
	go func() { _, err := repo.Finish(ctx, other.ID); finishResult <- err }()
	waitForRowLock(t, pool)
	close(releasePosition)
	require.NoError(t, <-positionResult)
	require.NoError(t, <-finishResult)
	got, err = repo.Get(ctx, other.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastPositionAt)
}

func TestStaleSQLKeysetAndConcurrentRefresh(t *testing.T) {
	pool, _ := positionsPool(t)
	repo := NewTripRepository(pool, time.Second)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	fresh := newPositionTrip(t, pool, 1, now)
	completed := newPositionTrip(t, pool, 2, now.Add(-time.Hour))
	_, err := repo.Finish(ctx, completed.ID)
	require.NoError(t, err)
	refreshed := newPositionTrip(t, pool, 3, now.Add(-time.Hour))
	_, err = savePosition(ctx, pool, domain.Position{TripID: refreshed.ID, RecordedAt: now})
	require.NoError(t, err)
	ids := make(map[uuid.UUID]bool, 250)
	for i := range 250 {
		trip := newPositionTrip(t, pool, i, now.Add(-time.Hour+time.Duration(i/2)*time.Second))
		ids[trip.ID] = true
	}
	var cursor *domain.StaleTrip
	var seen []domain.StaleTrip
	queries := 0
	for {
		batch, err := repo.StaleTrips(ctx, cursor, time.Minute, 100)
		require.NoError(t, err)
		queries++
		seen = append(seen, batch...)
		if len(batch) < 100 {
			break
		}
		last := batch[len(batch)-1]
		cursor = &last
	}
	require.Equal(t, 3, queries)
	require.Len(t, seen, 250)
	for i, trip := range seen {
		require.True(t, ids[trip.ID])
		delete(ids, trip.ID)
		if i > 0 {
			previous := seen[i-1]
			require.True(t, previous.PositionAt.Before(trip.PositionAt) || (previous.PositionAt.Equal(trip.PositionAt) && previous.ID.String() < trip.ID.String()))
		}
	}
	require.Empty(t, ids)
	require.NotEqual(t, fresh.ID, seen[0].ID)
	first, err := repo.StaleTrips(ctx, nil, time.Minute, 100)
	require.NoError(t, err)
	_, err = savePosition(ctx, pool, domain.Position{TripID: first[0].ID, RecordedAt: now})
	require.NoError(t, err)
	next, err := repo.StaleTrips(ctx, &first[len(first)-1], time.Minute, 100)
	require.NoError(t, err)
	require.Len(t, next, 100)
	for _, trip := range next {
		require.NotEqual(t, first[0].ID, trip.ID)
	}
}

func TestStaleIndexExplain(t *testing.T) {
	pool, _ := positionsPool(t)
	ctx := context.Background()
	// A realistic selective set; no enable_seqscan override.
	_, err := pool.Exec(ctx, `INSERT INTO trips (id,user_id,driver_id,start_latitude,start_longitude,end_latitude,end_longitude,price,status,started_at)
        SELECT gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),0,0,0,0,0,'active',now() - CASE WHEN i<=250 THEN interval '2 hours' ELSE interval '1 second' END
        FROM generate_series(1,30000) AS i`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ANALYZE trips")
	require.NoError(t, err)
	query, args, err := staleQuery(nil, time.Minute, 100)
	require.NoError(t, err)
	rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		lines = append(lines, line)
	}
	require.NoError(t, rows.Err())
	plan := strings.Join(lines, "\n")
	t.Log(plan)
	require.Contains(t, plan, "Index Scan using trips_active_position_idx")
	require.NotContains(t, plan, "Seq Scan")
}

func TestPositionExplicitTransactionRollback(t *testing.T) {
	pool, _ := positionsPool(t)
	repo := NewTripRepository(pool, time.Second)
	ctx := context.Background()
	trip := newPositionTrip(t, pool, 1, time.Now())
	original := errors.New("after insert")
	err := positionTx(pool).Do(ctx, func(ctx context.Context) error {
		if _, err := repo.SavePosition(ctx, domain.Position{TripID: trip.ID, RecordedAt: time.Now()}); err != nil {
			return err
		}
		return original
	})
	require.ErrorIs(t, err, original)
	got, err := repo.Get(ctx, trip.ID)
	require.NoError(t, err)
	require.Nil(t, got.LastPositionAt)
	points, err := repo.ListPositions(ctx, trip.ID)
	require.NoError(t, err)
	require.Empty(t, points)
}
