package builtin

import "github.com/gosuda/supersql-alpha/internal/model"

// TypeIO identifies PostgreSQL I/O operations and their type parameter. It does
// not establish that the selected functions have executable implementations.
type TypeIO struct {
	Input, Output model.FunctionID
	Parameter     model.TypeID
	Delimiter     byte
}

// LookupTypeIO returns I/O metadata only for pinned catalog types. Declared
// types require an immutable TypeResolver.
func LookupTypeIO(layout model.TypeLayout) (TypeIO, bool) {
	if layout.Type.ID == 0 {
		return TypeIO{}, false
	}
	info, known := LookupType(layout.Type.ID)
	if !known {
		return TypeIO{}, false
	}
	parameter := layout.Type.ID
	if info.Element != 0 {
		parameter = info.Element
	}
	return TypeIO{Input: info.Input, Output: info.Output, Parameter: parameter, Delimiter: info.Delimiter}, true
}
