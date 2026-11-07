package bind

import (
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

// Row expressions persist ASTs rather than constants. Record only the literal
// nodes the binder actually consumed; textual matches would rewrite shadowed calls.
func (b *binder) recordTemporalLiteral(input expression, target model.TypeLayout, value model.Value) error {
	if b.rowTemporalLiterals == nil || input.literal == nil || value.IsNull || !containsTemporalType(target) {
		return nil
	}
	constant := input.literal.GetAConst()
	if constant == nil || constant.GetSval() == nil {
		return nil
	}
	info, ok := builtin.LookupTypeIO(target)
	if !ok || info.Output == 0 {
		return missing("temporal literal output codec", input.position)
	}
	options := b.options.Builtin
	options.DateStyle = builtin.DateISO
	options.DateOrder = builtin.DateYMD
	// UTC is a serialization choice, not an invocation timezone default.
	options.TimeZone = builtin.TimeZoneContext{Name: "UTC", Fixed: true}
	options.ExtraFloatDigits = 1
	maximum := min(b.options.Limits.MaxBytes, uint64(math.MaxInt64))
	if b.rowTemporalBytes > maximum {
		return model.ErrValueBudget
	}
	maximum -= b.rowTemporalBytes
	function, err := builtin.ResolveTyped(b.ctx, info.Output, []model.TypeLayout{target}, b.primitive(2275), options, maximum)
	if err != nil {
		return err
	}
	remaining := maximum - function.OwnedBytes()
	text, err := function.Call(b.ctx, []model.Value{value}, builtin.CallBudget{OutputBytes: int64(remaining), MemoryBytes: remaining})
	if err != nil {
		return err
	}
	previous, exists := b.rowTemporalLiterals[input.literal]
	if exists {
		if previous != text.StrVal {
			return bindError("0A000", "temporal literal has inconsistent coercions", input.position)
		}
		return nil
	}
	if uint64(len(b.rowTemporalLiterals)) >= uint64(b.options.Limits.MaxOperands) || uint64(len(text.StrVal))+32 > maximum {
		return model.ErrValueBudget
	}
	b.rowTemporalBytes += uint64(len(text.StrVal)) + 32
	b.rowTemporalLiterals[input.literal] = text.StrVal
	return nil
}

func containsTemporalType(layout model.TypeLayout) bool {
	if family, ok := builtin.LookupRangeFamily(layout.Type.ID); ok {
		return family.Subtype == 1082 || family.Subtype == 1114 || family.Subtype == 1184
	}
	switch layout.Type.ID {
	case 1082, 1083, 1114, 1184, 1266:
		return true
	}
	for _, child := range layout.Children {
		if containsTemporalType(child) {
			return true
		}
	}
	return false
}
