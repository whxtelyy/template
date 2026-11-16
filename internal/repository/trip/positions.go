package trip

import (
	"context"
	"fmt"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/google/uuid"

	domain "github.com/whxtelyy/template/internal/domain/trip"
	"github.com/whxtelyy/template/internal/infra/postgres/transaction"
)

// SavePosition must run inside TxManager.Do. Updating first locks the same row
// as Finish; PostgreSQL rechecks active after waiting for a concurrent update.
func (r *TripRepository) SavePosition(ctx context.Context, p domain.Position) (domain.Position, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()
	executor := transaction.ExecutorFromContext(ctx, r.pool)
	query, args, err := squirrel.StatementBuilder.PlaceholderFormat(squirrel.Dollar).
		Update("trips").Set("last_position_at", squirrel.Expr("GREATEST(last_position_at, ?::timestamptz)", p.RecordedAt)).
		Set("updated_at", squirrel.Expr("now()")).Where(squirrel.Eq{"id": p.TripID, "status": domain.StatusActive}).ToSql()
	if err != nil {
		return domain.Position{}, fmt.Errorf("build position update: %w", err)
	}
	tag, err := executor.Exec(ctx, query, args...)
	if err != nil {
		return domain.Position{}, fmt.Errorf("update last position: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := r.Get(ctx, p.TripID); err != nil {
			return domain.Position{}, err
		}
		return domain.Position{}, domain.ErrTripCompleted
	}
	query, args, err = squirrel.StatementBuilder.PlaceholderFormat(squirrel.Dollar).
		Insert("trip_positions").Columns("trip_id", "latitude", "longitude", "recorded_at").
		Values(p.TripID, p.Coordinates.Latitude, p.Coordinates.Longitude, p.RecordedAt).Suffix("RETURNING id").ToSql()
	if err != nil {
		return domain.Position{}, fmt.Errorf("build position insert: %w", err)
	}
	if err := executor.QueryRow(ctx, query, args...).Scan(&p.ID); err != nil {
		return domain.Position{}, fmt.Errorf("insert position: %w", err)
	}
	return p, nil
}

func (r *TripRepository) ListPositions(ctx context.Context, id uuid.UUID) ([]domain.Position, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()
	executor := transaction.ExecutorFromContext(ctx, r.pool)
	query, args, err := squirrel.StatementBuilder.PlaceholderFormat(squirrel.Dollar).
		Select("id", "trip_id", "latitude", "longitude", "recorded_at").From("trip_positions").
		Where(squirrel.Eq{"trip_id": id}).OrderBy("recorded_at ASC", "id ASC").ToSql()
	if err != nil {
		return nil, fmt.Errorf("build positions query: %w", err)
	}
	rows, err := executor.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list positions: %w", err)
	}
	defer rows.Close()
	result := make([]domain.Position, 0)
	for rows.Next() {
		var p domain.Position
		if err := rows.Scan(&p.ID, &p.TripID, &p.Coordinates.Latitude, &p.Coordinates.Longitude, &p.RecordedAt); err != nil {
			return nil, fmt.Errorf("scan position: %w", err)
		}
		result = append(result, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read positions: %w", err)
	}
	return result, nil
}

func staleQuery(after *domain.StaleTrip, stale time.Duration, limit int) (string, []any, error) {
	// The literal status matches the partial index even with a generic plan.
	b := squirrel.StatementBuilder.PlaceholderFormat(squirrel.Dollar).
		Select("id", "driver_id", "COALESCE(last_position_at, started_at)").From("trips").
		Where("status = 'active'").Where(squirrel.Expr("COALESCE(last_position_at, started_at) < now() - ?::interval", fmt.Sprintf("%f seconds", stale.Seconds())))
	if after != nil {
		b = b.Where(squirrel.Expr("(COALESCE(last_position_at, started_at), id) > (?, ?)", after.PositionAt, after.ID))
	}
	// Squirrel.Limit embeds an integer; suffix keeps LIMIT parameterized too.
	return b.OrderBy("COALESCE(last_position_at, started_at) ASC", "id ASC").Suffix("LIMIT ?", limit).ToSql()
}

func (r *TripRepository) StaleTrips(ctx context.Context, after *domain.StaleTrip, stale time.Duration, limit int) ([]domain.StaleTrip, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()
	query, args, err := staleQuery(after, stale, limit)
	if err != nil {
		return nil, fmt.Errorf("build stale trips: %w", err)
	}
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query stale trips: %w", err)
	}
	defer rows.Close()
	result := make([]domain.StaleTrip, 0)
	for rows.Next() {
		var t domain.StaleTrip
		if err := rows.Scan(&t.ID, &t.DriverID, &t.PositionAt); err != nil {
			return nil, fmt.Errorf("scan stale trip: %w", err)
		}
		result = append(result, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read stale trips: %w", err)
	}
	return result, nil
}
