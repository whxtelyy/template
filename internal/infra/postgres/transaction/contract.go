package transaction

import (
	"context"

	"github.com/jackc/pgx/v5"
)

//go:generate go tool mockgen -source=contract.go -destination=mocks_test.go -package=transaction
//go:generate go tool mockgen -destination=pgx_mocks_test.go -package=transaction github.com/jackc/pgx/v5 Tx
type beginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}
