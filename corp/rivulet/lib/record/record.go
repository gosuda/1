package record

import (
	"fmt"
	"path/filepath"

	"github.com/cockroachdb/pebble"
)

const (
	NodeSetFile         = "node_set.db"
	FlowletFile         = "flowlet.db"
	ChainDefinitionFile = "chain_definition.db"
	ChainHistoryFile    = "chain_history.db"
)

type Record struct {
	nodeSetDB   *pebble.DB
	flowletDB   *pebble.DB
	chainDefDB  *pebble.DB
	chainHistDB *pebble.DB
}

type Config struct {
	Path   string
	Option *pebble.Options
}

func New(cfg *Config) (*Record, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	if cfg.Path == "" {
		return nil, fmt.Errorf("path cannot be empty")
	}

	if cfg.Option == nil {
		cfg.Option = &pebble.Options{}
	}

	nodeSetDB, err := pebble.Open(filepath.Join(cfg.Path, NodeSetFile), cfg.Option)
	if err != nil {
		return nil, fmt.Errorf("failed to open node set database: %w", err)
	}

	flowletDB, err := pebble.Open(filepath.Join(cfg.Path, FlowletFile), cfg.Option)
	if err != nil {
		return nil, fmt.Errorf("failed to open flowlet database: %w", err)
	}

	chainDefDB, err := pebble.Open(filepath.Join(cfg.Path, ChainDefinitionFile), cfg.Option)
	if err != nil {
		return nil, fmt.Errorf("failed to open chain definition database: %w", err)
	}

	chainHistDB, err := pebble.Open(filepath.Join(cfg.Path, ChainHistoryFile), cfg.Option)
	if err != nil {
		return nil, fmt.Errorf("failed to open chain history database: %w", err)
	}

	return &Record{
		nodeSetDB:   nodeSetDB,
		flowletDB:   flowletDB,
		chainDefDB:  chainDefDB,
		chainHistDB: chainHistDB,
	}, nil
}
