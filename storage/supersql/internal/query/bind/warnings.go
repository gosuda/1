package bind

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (b *binder) addWarning(warning model.Error) error {
	if len(b.warnings) == cap(b.warnings) {
		limit := min(uint64(b.options.Limits.MaxOperands), b.options.Limits.MaxBytes/uint64(unsafe.Sizeof(model.Error{}))-uint64(cap(b.warnings)))
		capacity := min(limit, max(uint64(cap(b.warnings))*2, 4))
		if capacity <= uint64(len(b.warnings)) {
			return bindError("54000", "binding warning limit exceeded", warning.Position)
		}
		grown := make([]model.Error, len(b.warnings), int(capacity))
		copy(grown, b.warnings)
		b.warnings = grown
	}
	b.warnings = append(b.warnings, warning)
	return nil
}
