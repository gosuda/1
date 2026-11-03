package bind

import (
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (b *binder) deferWriteError(err error) {
	if b.writeError == nil {
		b.writeError = err
	}
}
func (b *binder) deferArbiterError(err error) bool {
	if err == nil {
		return false
	}
	var sql *model.Error
	if !errors.As(err, &sql) {
		return false
	}
	switch sql.Code {
	case "42P10", "55000", "42809":
		if b.arbiterError == nil {
			b.arbiterError = err
		}
		return true
	}
	return false
}
