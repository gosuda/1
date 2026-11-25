package model

// ConflictArbiter identifies either the base primary key (Index zero) or an
// exact public unique-index descriptor version and generation.
type ConflictArbiter struct {
	Primary    bool
	Index      ObjectID
	Version    uint64
	Generation uint64
}

// ConflictOwner owns the encoded base primary key of a conflicting row.
type ConflictOwner struct {
	Arbiter ConflictArbiter
	Key     []byte
}
