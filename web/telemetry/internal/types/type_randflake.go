package types

type RandflakeLease struct {
	LeaseID   [16]byte
	NodeID    int64
	CreatedAt int64
	ExpiresAt int64
}
