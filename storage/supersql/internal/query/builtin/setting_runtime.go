package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileSettings(signature Signature) scalarKernel {
	switch signature.ID {
	case 2077:
		return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
			return currentSetting(ctx, arguments[0].StrVal, false, budget)
		}
	case 3294:
		return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
			return currentSetting(ctx, arguments[0].StrVal, arguments[1].IntVal != 0, budget)
		}
	case 2078:
		return setConfig
	default:
		return nil
	}
}

func currentSetting(ctx context.Context, name string, missingOK bool, budget CallBudget) (model.Value, error) {
	if budget.Settings == nil {
		return model.Value{}, sqlError("55000", "SQL setting runtime is unavailable")
	}
	value, err := budget.Settings.CurrentSetting(ctx, name, missingOK)
	if err != nil {
		return model.Value{}, err
	}
	return settingResult(value, budget)
}

func setConfig(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	if arguments[0].IsNull {
		return model.Value{}, sqlError("22004", "SET requires parameter name")
	}
	if budget.Settings == nil {
		return model.Value{}, sqlError("55000", "SQL setting runtime is unavailable")
	}
	var value *string
	if !arguments[1].IsNull {
		value = &arguments[1].StrVal
	}
	local := !arguments[2].IsNull && arguments[2].IntVal != 0
	if err := outputBudget(0, budget.OutputBytes); err != nil {
		return model.Value{}, err
	}
	maximum := min(uint64(budget.OutputBytes), budget.MemoryBytes, uint64((1<<30)-5))
	result, err := budget.Settings.SetConfig(ctx, arguments[0].StrVal, value, local, maximum)
	if err != nil {
		return model.Value{}, err
	}
	return settingResult(result, budget)
}

func settingResult(value model.Value, budget CallBudget) (model.Value, error) {
	if value.Type != model.TypeString {
		return model.Value{}, sqlError("XX000", "setting runtime returned invalid value")
	}
	if value.IsNull {
		return value, nil
	}
	if err := outputBudget(int64(len(value.StrVal)), budget.OutputBytes); err != nil {
		return model.Value{}, err
	}
	if uint64(len(value.StrVal)) > budget.MemoryBytes {
		return model.Value{}, sqlError("54000", "builtin invocation memory budget exceeded")
	}
	return value, nil
}
