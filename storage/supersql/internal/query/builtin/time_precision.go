package builtin

import (
	"fmt"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// TimePrecision validates a declared TIME(p), retaining PostgreSQL's clamp warning.
func TimePrecision(precision int32) (int32, *model.Error, error) {
	return temporalPrecision(precision, "TIME", "")
}

// TimestampPrecision validates a declared timestamp precision and retains its clamp warning.
func TimestampPrecision(precision int32, withTimeZone bool) (int32, *model.Error, error) {
	suffix := ""
	if withTimeZone {
		suffix = " WITH TIME ZONE"
	}
	return temporalPrecision(precision, "TIMESTAMP", suffix)
}

func temporalPrecision(precision int32, kind, suffix string) (int32, *model.Error, error) {
	if precision < 0 {
		return 0, nil, sqlError("22023", fmt.Sprintf("%s(%d)%s precision must not be negative", kind, precision, suffix))
	}
	if precision > 6 {
		return 6, &model.Error{Code: "22023", Message: fmt.Sprintf("%s(%d)%s precision reduced to maximum allowed, 6", kind, precision, suffix)}, nil
	}
	return precision, nil, nil
}

func roundTimePrecision(microseconds int64, precision int64) int64 {
	if precision < 0 || precision > 6 {
		return microseconds
	}
	scale := int64(1)
	for remaining := int64(6) - precision; remaining > 0; remaining-- {
		scale *= 10
	}
	return (microseconds + scale/2) / scale * scale
}
