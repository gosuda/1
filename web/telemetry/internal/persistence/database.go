package persistence

import (
	"context"
	"database/sql"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"telemetry.gosuda.org/telemetry/internal/persistence/database"
	"telemetry.gosuda.org/telemetry/internal/types"
)

type PersistenceClientConfig struct {
	DSN             string        `env:"DATABASE_DSN"`
	ConnMaxIdleTime time.Duration `env:"DATABASE_CONN_MAX_IDLE_TIME"`
	ConnMaxLifetime time.Duration `env:"DATABASE_CONN_MAX_LIFETIME"`
	MaxIdleConns    int           `env:"DATABASE_MAX_IDLE_CONNS"`
	MaxOpenConns    int           `env:"DATABASE_MAX_OPEN_CONNS"`
}

type PersistenceClient struct {
	pool *sql.DB
	db   *database.Queries
}

var _ types.PersistenceService = (*PersistenceClient)(nil)

func NewPersistenceClient(ctx context.Context, config *PersistenceClientConfig) (*PersistenceClient, error) {
	db, err := sql.Open("mysql", config.DSN)
	if err != nil {
		return nil, err
	}

	db.SetConnMaxIdleTime(config.ConnMaxIdleTime)
	db.SetConnMaxLifetime(config.ConnMaxLifetime)
	db.SetMaxIdleConns(config.MaxIdleConns)
	db.SetMaxOpenConns(config.MaxOpenConns)

	err = db.PingContext(ctx)
	if err != nil {
		return nil, err
	}

	dbtx := database.New(db)

	return &PersistenceClient{pool: db, db: dbtx}, nil
}

func (g *PersistenceClient) Close() error {
	err := g.pool.Close()
	if err != nil {
		return err
	}

	return nil
}
