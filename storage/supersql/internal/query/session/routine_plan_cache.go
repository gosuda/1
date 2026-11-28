package session

import (
	"bytes"
	"crypto/sha256"
	"slices"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const (
	routineStatementCacheMaxEntries = 64
	routineStatementCacheMaxASTBytes = 64 << 10
)

type routineStatementCacheKey struct {
	hash             [32]byte
	astBytes         []byte
	allowControl     bool
	identity         txn.CatalogReadIdentity
	temporary        uint64
	searchPath       []string
	currentRole      string
	semantics        builtin.ResolveOptions
	workingMemory    uint64
	parameterLayouts []model.TypeLayout
}

type routineStatementCacheEntry struct {
	key            routineStatementCacheKey
	bound          *bind.BoundStatement
	prepared       *Prepared
	scanDeps       []txn.ReadDependency
	definitions    []procedural.Definition
	hasScans       bool
	hasCatalogScan bool
	hasParams      bool
}

// routineStatementCache retains bound statements and parameter-independent
// compiled plans across repeated SQL/PLpgSQL loop iterations. Every entry is
// keyed by CatalogReadIdentity, temporary-object version, search path,
// semantic settings, working memory, parameter layouts, and canonical AST
// bytes so DDL, setting changes, or AST rewrites invalidate automatically.
type routineStatementCache struct {
	mu      sync.Mutex
	entries []routineStatementCacheEntry
}

func makeRoutineStatementCacheKey(statement *zpgparse.RawStmt, allowControl bool, identity txn.CatalogReadIdentity, temporary uint64, searchPath []string, currentRole string, semantics builtin.ResolveOptions, workingMemory uint64, parameterLayouts []model.TypeLayout) (routineStatementCacheKey, bool) {
	if statement == nil || statement.Stmt == nil || identity == (txn.CatalogReadIdentity{}) {
		return routineStatementCacheKey{}, false
	}
	astBytes, err := (proto.MarshalOptions{Deterministic: true}).Marshal(statement.Stmt)
	if err != nil || len(astBytes) == 0 || len(astBytes) > routineStatementCacheMaxASTBytes {
		return routineStatementCacheKey{}, false
	}
	semantics.Types = nil
	return routineStatementCacheKey{
		hash:             sha256.Sum256(astBytes),
		astBytes:         astBytes,
		allowControl:     allowControl,
		identity:         identity,
		temporary:        temporary,
		searchPath:       slices.Clone(searchPath),
		currentRole:      currentRole,
		semantics:        semantics,
		workingMemory:    workingMemory,
		parameterLayouts: cloneTypeLayouts(parameterLayouts),
	}, true
}

func (a routineStatementCacheKey) equal(b routineStatementCacheKey) bool {
	if a.hash != b.hash || a.allowControl != b.allowControl || a.identity != b.identity || a.temporary != b.temporary || a.currentRole != b.currentRole || a.workingMemory != b.workingMemory {
		return false
	}
	if len(a.parameterLayouts) != len(b.parameterLayouts) || !slices.Equal(a.searchPath, b.searchPath) {
		return false
	}
	if !a.semantics.SameSemantics(b.semantics) {
		return false
	}
	for i := range a.parameterLayouts {
		if !typeLayoutsEqualWithFields(a.parameterLayouts[i], b.parameterLayouts[i]) {
			return false
		}
	}
	return bytes.Equal(a.astBytes, b.astBytes)
}

func typeLayoutsEqualWithFields(a, b model.TypeLayout) bool {
	if a.Type != b.Type || len(a.Children) != len(b.Children) || !slices.Equal(a.FieldNames, b.FieldNames) {
		return false
	}
	for i := range a.Children {
		if !typeLayoutsEqualWithFields(a.Children[i], b.Children[i]) {
			return false
		}
	}
	return true
}

func (c *routineStatementCache) load(key routineStatementCacheKey) (routineStatementCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for index := range c.entries {
		if !c.entries[index].key.equal(key) {
			continue
		}
		entry := c.entries[index]
		copy(c.entries[1:index+1], c.entries[:index])
		c.entries[0] = entry
		return entry, true
	}
	return routineStatementCacheEntry{}, false
}

func (c *routineStatementCache) store(entry routineStatementCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for index := range c.entries {
		if c.entries[index].key.equal(entry.key) {
			copy(c.entries[1:index+1], c.entries[:index])
			c.entries[0] = entry
			return
		}
	}
	if len(c.entries) < routineStatementCacheMaxEntries {
		c.entries = append(c.entries, routineStatementCacheEntry{})
	}
	copy(c.entries[1:], c.entries[:len(c.entries)-1])
	c.entries[0] = entry
}

func boundRoutineGraphShape(bound *bind.BoundStatement) (hasScans, hasCatalogScan, hasParams bool) {
	if bound == nil {
		return false, false, false
	}
	graph := bound.Graph()
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
		relation, ok := graph.Rel(id)
		if !ok {
			continue
		}
		if relation.Kind == ir.Scan && relation.Occurrence != 0 {
			hasScans = true
		}
		if relation.Kind == ir.CatalogScan {
			hasCatalogScan = true
		}
	}
	for id := ir.ScalarID(1); uint32(id) <= graph.ScalarCount(); id++ {
		scalar, ok := graph.Scalar(id)
		if ok && scalar.Kind == ir.Param {
			hasParams = true
			break
		}
	}
	return hasScans, hasCatalogScan, hasParams
}
