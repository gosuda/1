package session

import (
	"slices"
	"sync"

	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
)

// declaredRoutineKey names every input that determines the routine definitions a
// declared-type runtime discovers: the catalog content the discovery snapshot
// exposes, the session's temporary-object version, the limits that bound
// decoding, and the exact type descriptors the discovery was seeded from.
type declaredRoutineKey struct {
	identity    txn.CatalogReadIdentity
	temporary   uint64
	descriptors model.DescriptorLimits
	routines    procedural.Limits
	seeds       string
}

// declaredRoutineEntry owns one discovered definition set. Entries are never
// mutated after storage; every read returns a deep copy.
type declaredRoutineEntry struct {
	key         declaredRoutineKey
	definitions []procedural.Definition
}

// declaredRoutineCache retains the most recent discovered sets. The bound is a
// constant, and the mutex is never held across catalog I/O.
type declaredRoutineCache struct {
	mu      sync.Mutex
	entries []declaredRoutineEntry
}

const declaredRoutineEntries = 4

func (cache *declaredRoutineCache) load(key declaredRoutineKey) ([]procedural.Definition, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for index := range cache.entries {
		if cache.entries[index].key != key {
			continue
		}
		entry := cache.entries[index]
		copy(cache.entries[1:index+1], cache.entries[:index])
		cache.entries[0] = entry
		return cloneProceduralDefinitions(entry.definitions), true
	}
	return nil, false
}

func (cache *declaredRoutineCache) store(key declaredRoutineKey, definitions []procedural.Definition) {
	entry := declaredRoutineEntry{key: key, definitions: cloneProceduralDefinitions(definitions)}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for index := range cache.entries {
		if cache.entries[index].key == key {
			copy(cache.entries[1:index+1], cache.entries[:index])
			cache.entries[0] = entry
			return
		}
	}
	if len(cache.entries) < declaredRoutineEntries {
		cache.entries = append(cache.entries, declaredRoutineEntry{})
	}
	copy(cache.entries[1:], cache.entries[:len(cache.entries)-1])
	cache.entries[0] = entry
}

// cloneProceduralDefinitions deep-copies every slice, pointer, and nested layout
// a definition owns, so no caller and no cached entry share mutable storage.
func cloneProceduralDefinitions(definitions []procedural.Definition) []procedural.Definition {
	if definitions == nil {
		return nil
	}
	result := make([]procedural.Definition, len(definitions))
	for i, definition := range definitions {
		result[i] = definition
		metadata := &result[i].Metadata
		metadata.Body.Dependencies = slices.Clone(definition.Metadata.Body.Dependencies)
		metadata.Body.Encoding = slices.Clone(definition.Metadata.Body.Encoding)
		metadata.Result = cloneDiscoveredLayout(definition.Metadata.Result)
		metadata.Settings = slices.Clone(definition.Metadata.Settings)
		if definition.Metadata.Parameters != nil {
			metadata.Parameters = make([]model.RoutineParameter, len(definition.Metadata.Parameters))
			for j, parameter := range definition.Metadata.Parameters {
				metadata.Parameters[j] = parameter
				metadata.Parameters[j].Layout = cloneDiscoveredLayout(parameter.Layout)
				if parameter.Default != nil {
					defaultValue := *parameter.Default
					defaultValue.Dependencies = slices.Clone(parameter.Default.Dependencies)
					defaultValue.Encoding = slices.Clone(parameter.Default.Encoding)
					metadata.Parameters[j].Default = &defaultValue
				}
			}
		}
	}
	return result
}

// cloneDiscoveredLayout deep-copies a type layout tree, including presentation
// names, so a cached layout never aliases a caller's child slices.
func cloneDiscoveredLayout(layout model.TypeLayout) model.TypeLayout {
	result := layout
	result.FieldNames = slices.Clone(layout.FieldNames)
	if layout.Children != nil {
		result.Children = make([]model.TypeLayout, len(layout.Children))
		for i, child := range layout.Children {
			result.Children[i] = cloneDiscoveredLayout(child)
		}
	}
	return result
}

// declaredRoutineSeeds names the exact type descriptors a discovery was seeded
// from: the identity and version of each definition, in order. The discovery
// reads only what these descriptors name, so equal seeds under an equal catalog
// identity discover equal definitions.
func declaredRoutineSeeds(types []builtin.TypeDefinition) string {
	buffer := make([]byte, 0, len(types)*16)
	for _, definition := range types {
		id := uint64(definition.Descriptor.ID())
		version := definition.Descriptor.Version()
		for shift := 56; shift >= 0; shift -= 8 {
			buffer = append(buffer, byte(id>>shift))
		}
		for shift := 56; shift >= 0; shift -= 8 {
			buffer = append(buffer, byte(version>>shift))
		}
	}
	return string(buffer)
}
