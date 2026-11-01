package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileAdvisory(signature Signature) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		if budget.Advisory == nil {
			return model.Value{}, sqlError("55000", "advisory lock runtime is unavailable")
		}
		if signature.ID == 2892 {
			if err := budget.Advisory.UnlockAllAdvisory(ctx); err != nil {
				return model.Value{}, err
			}
			return model.StringVal(""), nil
		}
		key := advisoryKey(signature.ID, arguments)
		mode := model.AdvisoryExclusive
		switch signature.ID {
		case 2881, 2883, 2885, 2887, 2889, 2891, 3090, 3092, 3094, 3096:
			mode = model.AdvisoryShared
		}
		switch signature.ID {
		case 2884, 2885, 2890, 2891:
			unlocked, err := budget.Advisory.UnlockAdvisory(ctx, key, mode)
			if err != nil {
				return model.Value{}, err
			}
			return model.BoolVal(unlocked), nil
		}
		scope := model.AdvisorySessionScope
		if signature.ID >= 3089 {
			scope = model.AdvisoryTransactionScope
		}
		wait := true
		switch signature.ID {
		case 2882, 2883, 2888, 2889, 3091, 3092, 3095, 3096:
			wait = false
		}
		acquired, err := budget.Advisory.AcquireAdvisory(ctx, model.AdvisoryLockRequest{Key: key, Mode: mode, Scope: scope, Wait: wait})
		if err != nil {
			return model.Value{}, err
		}
		if !wait {
			return model.BoolVal(acquired), nil
		}
		if !acquired {
			return model.Value{}, sqlError("XX000", "blocking advisory lock was not acquired")
		}
		return model.StringVal(""), nil
	}
}

func advisoryKey(id model.FunctionID, arguments []model.Value) model.AdvisoryKey {
	switch id {
	case 2886, 2887, 2888, 2889, 2890, 2891, 3093, 3094, 3095, 3096:
		return model.AdvisoryKey{Space: model.AdvisoryIntPairKey, Value: uint64(uint32(arguments[0].IntVal))<<32 | uint64(uint32(arguments[1].IntVal))}
	default:
		return model.AdvisoryKey{Space: model.AdvisoryBigIntKey, Value: uint64(arguments[0].IntVal)}
	}
}
