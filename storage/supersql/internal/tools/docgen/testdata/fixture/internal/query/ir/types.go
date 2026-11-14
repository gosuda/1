package ir

// Limits bounds one intermediate-representation graph.
type Limits struct {
	// MaxNodes is graph-wide; MaxOperands bounds each typed operand arena.
	MaxNodes, MaxOperands, MaxSlots, MaxDepth uint32
	MaxBytes                                  uint64
	SourceBytes                               uint32
}
