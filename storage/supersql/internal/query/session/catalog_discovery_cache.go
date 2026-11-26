package session

import (
	"slices"
	"sync"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// catalogDiscovery names the session state a discovered dependency set is cached
// under. A nil cache disables caching; every discovery then walks the catalog.
type catalogDiscovery struct {
	cache     *catalogDiscoveryCache
	temporary uint64
}

// catalogDiscoveryKey names every input that determines the discovered suffix:
// the catalog content the discovery snapshot exposes (CatalogReadIdentity covers
// committed commits, the transaction's staged catalog overlay, and the snapshot's
// own overlay) and the session's temporary-object version. The base dependencies
// are not part of the key: the walk's results do not depend on them, and the
// caller prepends them unchanged on every call.
type catalogDiscoveryKey struct {
	identity  txn.CatalogReadIdentity
	temporary uint64
}

// catalogDiscoveryEntry owns one discovered suffix: the dependencies discovery
// appends to base, and the budget those dependencies charge. Entries are never
// mutated after they are stored; callers receive clones.
type catalogDiscoveryEntry struct {
	key     catalogDiscoveryKey
	suffix  []txn.ReadDependency
	content uint64
}

// catalogDiscoveryCache retains the most recent discovered suffixes. The bound is
// a constant, so retained metadata is bounded. The mutex is never held across
// catalog I/O.
type catalogDiscoveryCache struct {
	mu      sync.Mutex
	entries []catalogDiscoveryEntry
}

// catalogDiscoveryEntries bounds retained suffixes. A statement uses one identity
// at a time; the allowance keeps the previous set for a nested statement.
const catalogDiscoveryEntries = 4

func (cache *catalogDiscoveryCache) load(key catalogDiscoveryKey) (catalogDiscoveryEntry, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for index := range cache.entries {
		if cache.entries[index].key != key {
			continue
		}
		entry := cache.entries[index]
		copy(cache.entries[1:index+1], cache.entries[:index])
		cache.entries[0] = entry
		return entry, true
	}
	return catalogDiscoveryEntry{}, false
}

func (cache *catalogDiscoveryCache) store(entry catalogDiscoveryEntry) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for index := range cache.entries {
		if cache.entries[index].key == entry.key {
			copy(cache.entries[1:index+1], cache.entries[:index])
			cache.entries[0] = entry
			return
		}
	}
	if len(cache.entries) < catalogDiscoveryEntries {
		cache.entries = append(cache.entries, catalogDiscoveryEntry{})
	}
	copy(cache.entries[1:], cache.entries[:len(cache.entries)-1])
	cache.entries[0] = entry
}

// catalogDiscovery returns this session's discovery cache with the temporary
// object version the cached suffixes are keyed by.
func (transactions *Transactions) catalogDiscovery() catalogDiscovery {
	return catalogDiscovery{cache: &transactions.discoveries, temporary: transactions.named.overlayVersion()}
}

// cloneCatalogDependencies deep-copies dependencies so no caller shares storage
// with a cached suffix.
func cloneCatalogDependencies(dependencies []txn.ReadDependency) []txn.ReadDependency {
	if dependencies == nil {
		return nil
	}
	result := make([]txn.ReadDependency, len(dependencies))
	for i, dependency := range dependencies {
		result[i].Target = dependency.Target
		result[i].Occurrence = dependency.Occurrence
		result[i].KeyLayout = make([]storage.KeyColumn, len(dependency.KeyLayout))
		for j, column := range dependency.KeyLayout {
			result[i].KeyLayout[j] = column
			result[i].KeyLayout[j].Children = cloneTypeLayouts(column.Children)
		}
		result[i].Spans = make([]storage.KeySpan, len(dependency.Spans))
		for j, span := range dependency.Spans {
			result[i].Spans[j] = storage.KeySpan{
				Lower: cloneKeyBound(span.Lower),
				Upper: cloneKeyBound(span.Upper),
			}
		}
	}
	return result
}

func cloneKeyBound(bound storage.KeyBound) storage.KeyBound {
	result := storage.KeyBound{Inclusive: bound.Inclusive, Unbounded: bound.Unbounded}
	if bound.Values != nil {
		result.Values = make([]model.Value, len(bound.Values))
		for i, value := range bound.Values {
			result.Values[i] = value.Clone()
		}
	}
	return result
}

func cloneTypeLayouts(layouts []model.TypeLayout) []model.TypeLayout {
	if layouts == nil {
		return nil
	}
	result := make([]model.TypeLayout, len(layouts))
	for i, layout := range layouts {
		result[i] = layout
		result[i].Children = cloneTypeLayouts(layout.Children)
		result[i].FieldNames = slices.Clone(layout.FieldNames)
	}
	return result
}
