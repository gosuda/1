package persistence

import (
	"context"
	"database/sql"
	"errors"

	"telemetry.gosuda.org/telemetry/internal/persistence/database"
)

var (
	ErrUnexpectedPingResult = errors.New("persistence: unexpected ping result")
)

func (g *PersistenceClient) Ping(ctx context.Context) error {
	tx, err := g.pool.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
	})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	ret, err := database.New(tx).Ping(ctx)
	if err != nil {
		return err
	}

	if ret != 1 {
		return ErrUnexpectedPingResult
	}

	return tx.Commit()
}
