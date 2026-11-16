package transaction

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/mock/gomock"
)

func TestPanicRollback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		logged bool
	}{
		{"failed", errors.New("rollback unavailable"), true}, {"closed", pgx.ErrTxClosed, false}, {"success", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			ctrl := gomock.NewController(t)
			tx := NewMockTx(ctrl)
			pool := NewMockbeginner(ctrl)
			pool.EXPECT().BeginTx(gomock.Any(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted}).Return(tx, nil)
			tx.EXPECT().Rollback(gomock.Any()).DoAndReturn(func(ctx context.Context) error {
				if ctx.Err() != nil {
					t.Errorf("rollback context: %v", ctx.Err())
				}
				return tc.err
			})
			m := NewManager(pool, pgx.ReadCommitted, time.Second, slog.New(slog.NewJSONHandler(&logs, nil)))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			original := errors.New("original panic")
			func() {
				defer func() {
					if got := recover(); got != original {
						t.Errorf("panic=%v", got)
					}
				}()
				_ = m.Do(ctx, func(inner context.Context) error {
					return m.Do(inner, func(context.Context) error { cancel(); panic(original) })
				})
			}()
			if strings.Contains(logs.String(), "rollback after panic failed") != tc.logged {
				t.Fatalf("logs=%s", logs.String())
			}
		})
	}
}

func TestTransactionOutcomes(t *testing.T) {
	original := errors.New("function failed")
	commitErr := errors.New("commit failed")
	for _, tc := range []struct {
		name                          string
		fnErr, commitErr, rollbackErr error
	}{
		{name: "commit"}, {name: "commit failure", commitErr: commitErr}, {name: "rollback", fnErr: original}, {name: "rollback failure", fnErr: original, rollbackErr: errors.New("rollback failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			tx := NewMockTx(ctrl)
			pool := NewMockbeginner(ctrl)
			pool.EXPECT().BeginTx(gomock.Any(), gomock.Any()).Return(tx, nil)
			if tc.fnErr != nil {
				tx.EXPECT().Rollback(gomock.Any()).Return(tc.rollbackErr)
			} else {
				tx.EXPECT().Commit(gomock.Any()).Return(tc.commitErr)
			}
			m := NewManager(pool, pgx.ReadCommitted, time.Second, slog.Default())
			err := m.Do(context.Background(), func(context.Context) error { return tc.fnErr })
			want := tc.fnErr
			if want == nil {
				want = tc.commitErr
			}
			if !errors.Is(err, want) {
				t.Fatalf("error=%v want=%v", err, want)
			}
		})
	}
}
