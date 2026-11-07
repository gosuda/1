package builtin

import (
	"context"
	"math"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileDateOutput(signature Signature, options ResolveOptions) scalarKernel {
	if signature.ID != 1085 {
		return nil
	}
	style, order := options.DateStyle, options.DateOrder
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		days := args[0].IntVal
		if !validDateDatum(days) {
			return model.Value{}, sqlError("22008", "date out of range")
		}
		// DateADT outputs can exceed the input domain after timezone projection.
		var scratch [14]byte
		output := scratch[:0]
		switch days {
		case math.MinInt32:
			output = append(output, "-infinity"...)
		case math.MaxInt32:
			output = append(output, "infinity"...)
		default:
			year, month, day := dateCalendar(days)
			bc := year <= 0
			if bc {
				year = 1 - year
			}
			fields := [3]int64{month, day, year}
			widths := [3]int{2, 2, 4}
			separator := byte('-')
			switch style {
			case DateISO:
				fields = [3]int64{year, month, day}
				widths = [3]int{4, 2, 2}
			case DateGerman:
				fields = [3]int64{day, month, year}
				separator = '.'
			case DateSQL, DatePostgres:
				if order == DateDMY {
					fields[0], fields[1] = day, month
				}
				if style == DateSQL {
					separator = '/'
				}
			}
			for index, field := range fields {
				if index != 0 {
					output = append(output, separator)
				}
				for digits, threshold := 1, int64(10); digits < widths[index]; digits, threshold = digits+1, threshold*10 {
					if field < threshold {
						output = append(output, '0')
					}
				}
				output = strconv.AppendInt(output, field, 10)
			}
			if bc {
				output = append(output, " BC"...)
			}
		}
		if err := outputBudget(int64(len(output)), budget.OutputBytes); err != nil {
			return model.Value{}, err
		}
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		return model.StringVal(string(output)), nil
	}
}
