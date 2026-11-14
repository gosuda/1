package neosepia

// Limits is the fixture backend resource envelope.
type Limits struct {
	ResidentBytes, MaxFiles, MaxPins uint64
	MetadataBytes, RecoveryFiles     uint64
	BlockRows                        int
	Builders                         int
	Plans                            int
}

// DefaultLimits returns the fixture defaults.
func DefaultLimits() Limits {
	return Limits{
		ResidentBytes: 64 << 20, MaxFiles: 65536, MaxPins: 512,
		MetadataBytes: 8 << 20, RecoveryFiles: 1 << 20,
		BlockRows: 1024, Builders: 8, Plans: 128,
	}
}
