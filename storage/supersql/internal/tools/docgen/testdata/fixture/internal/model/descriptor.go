package model

// DescriptorLimits bounds one logical descriptor serialization.
type DescriptorLimits struct {
	MaxBytes uint64
	MaxItems int
	MaxDepth int
}
