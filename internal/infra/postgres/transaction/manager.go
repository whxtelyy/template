package transaction

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Executor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type contextKey struct{}

type Manager struct {
	pool    beginner
	iso     pgx.TxIsoLevel
	timeout time.Duration
	logger  *slog.Logger
}

func NewManager(pool beginner, iso pgx.TxIsoLevel, timeout time.Duration, logger *slog.Logger) *Manager {
	return &Manager{pool: pool, iso: iso, timeout: timeout, logger: logger}
}

func (m *Manager) Do(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if _, ok := ctx.Value(contextKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}

	beginCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	tx, err := m.pool.BeginTx(beginCtx, pgx.TxOptions{IsoLevel: m.iso})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	txCtx := context.WithValue(ctx, contextKey{}, tx)
	defer func() {
		if recovered := recover(); recovered != nil {
			rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), m.timeout)
			rollbackErr := tx.Rollback(rollbackCtx)
			cancelRollback()
			if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				m.logger.ErrorContext(ctx, "rollback after panic failed", "error", rollbackErr)
			}
			panic(recovered)
		}
	}()

	if err = fn(txCtx); err != nil {
		rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), m.timeout)
		rollbackErr := tx.Rollback(rollbackCtx)
		cancelRollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			return fmt.Errorf("transaction function failed: %w; rollback failed: %v", err, rollbackErr)
		}
		return err
	}

	commitCtx, cancelCommit := context.WithTimeout(ctx, m.timeout)
	if err = tx.Commit(commitCtx); err != nil {
		cancelCommit()
		return fmt.Errorf("commit transaction: %w", err)
	}
	cancelCommit()
	return nil
}

func ExecutorFromContext(ctx context.Context, fallback Executor) Executor {
	if executor, ok := ctx.Value(contextKey{}).(pgx.Tx); ok {
		return executor
	}
	return fallback
}
