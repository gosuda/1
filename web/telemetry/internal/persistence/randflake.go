package persistence

import (
	"context"
	"database/sql"
	"errors"
	"math/rand"
	"time"

	"github.com/google/uuid"
	"telemetry.gosuda.org/telemetry/internal/persistence/database"
	"telemetry.gosuda.org/telemetry/internal/types"
)

const (
	_RANDFLAKE_NODE_BITS = 17
	_RANDFLAKE_MAX_NODE  = (1 << _RANDFLAKE_NODE_BITS) - 1

	_RANDFLAKE_LEASE_TTL   = int64(time.Minute * 10)
	_RANDFLAKE_SAFE_WINDOW = int64(time.Second * 30)
)

var (
	ErrUnsafeRandflakeLease = errors.New("persistence: unsafe randflake lease")
)

func (g *PersistenceClient) RandflakeGC(ctx context.Context) error {
	t := time.Now().UnixNano() - _RANDFLAKE_SAFE_WINDOW
	// delete all expired leases
	return g.db.RandflakeGC(ctx, t)
}

func (g *PersistenceClient) RandflakeLeaseCreate(ctx context.Context) (*types.RandflakeLease, error) {
	tx, err := g.pool.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	leaseID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}

	nodeID := rand.Int63n(1 << _RANDFLAKE_NODE_BITS)
	if nodeID > _RANDFLAKE_MAX_NODE {
		nodeID = nodeID & _RANDFLAKE_MAX_NODE
	}

	now := time.Now()
	createdAt := now.UnixNano()
	expiresAt := createdAt + _RANDFLAKE_LEASE_TTL

	err = database.New(tx).RandflakeLeaseCreate(ctx, database.RandflakeLeaseCreateParams{
		Uuid:      leaseID[:],
		NodeID:    nodeID,
		CreatedAt: createdAt,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return nil, err
	}

	err = tx.Commit()
	if err != nil {
		return nil, err
	}

	return &types.RandflakeLease{
		LeaseID:   leaseID,
		NodeID:    nodeID,
		CreatedAt: createdAt,
		ExpiresAt: expiresAt,
	}, nil
}

func (g *PersistenceClient) RandflakeLeaseExtend(ctx context.Context, prev *types.RandflakeLease) (*types.RandflakeLease, error) {
	now := time.Now().UnixNano()
	expiresAt := now + _RANDFLAKE_LEASE_TTL

	if prev.ExpiresAt-_RANDFLAKE_SAFE_WINDOW < now {
		return nil, ErrUnsafeRandflakeLease
	}

	tx, err := g.pool.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	err = database.New(tx).RandflakeLeaseExtend(ctx, database.RandflakeLeaseExtendParams{
		Uuid:      prev.LeaseID[:],
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return nil, err
	}

	err = tx.Commit()
	if err != nil {
		return nil, err
	}

	return &types.RandflakeLease{
		LeaseID:   prev.LeaseID,
		NodeID:    prev.NodeID,
		CreatedAt: prev.CreatedAt,
		ExpiresAt: expiresAt,
	}, nil
}
