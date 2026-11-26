package record

import (
	"encoding/json"
	"fmt"
)

type ChainDefinition struct {
	Name       string   `json:"name"`        // Unique identifier for the chain
	EntryPoint string   `json:"entry_point"` // The entry point of the chain, typically a flowlet name
	Flowlets   []string `json:"flowlets"`    // Requirement list of flowlets that are part of the chain
}

func (r *Record) GetChainDefinition(name string) (*ChainDefinition, error) {
	var chainDef ChainDefinition
	value, closer, err := r.chainDefDB.Get([]byte(name))
	if err != nil {
		return nil, fmt.Errorf("failed to get chain definition %s: %w", name, err)
	}
	defer closer.Close()

	cd := &ChainDefinition{}
}
