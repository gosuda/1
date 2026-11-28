package session

import (
	"context"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type settingRuntime struct {
	owner     *Transactions
	operation *sessionTransactionOperation
}

func (transactions *Transactions) settingRuntime(ctx context.Context) settingRuntime {
	operation, _ := ctx.Value(sessionTransactionKey{}).(*sessionTransactionOperation)
	return settingRuntime{owner: transactions, operation: operation}
}

func (runtime settingRuntime) check(ctx context.Context) error {
	if runtime.operation == nil || !runtime.operation.active.Load() || ctx.Value(sessionTransactionKey{}) != runtime.operation {
		return errSessionTransactionReentry
	}
	return ctx.Err()
}

func (runtime settingRuntime) CurrentSetting(ctx context.Context, name string, missingOK bool) (model.Value, error) {
	runtime.owner.settingMu.Lock()
	defer runtime.owner.settingMu.Unlock()
	if err := runtime.check(ctx); err != nil {
		return model.Value{}, err
	}
	return runtime.owner.settings.CurrentSetting(name, missingOK)
}

func (runtime settingRuntime) SetConfig(ctx context.Context, name string, value *string, local bool, maxResultBytes uint64) (model.Value, error) {
	runtime.owner.settingMu.Lock()
	defer runtime.owner.settingMu.Unlock()
	if err := runtime.check(ctx); err != nil {
		return model.Value{}, err
	}
	if err := runtime.owner.markDataAccess(); err != nil {
		return model.Value{}, err
	}
	staged := *runtime.owner.settings
	result, err := staged.SetConfig(ctx, name, value, local)
	if err != nil {
		return model.Value{}, err
	}
	if uint64(len(result.StrVal)) > maxResultBytes {
		return model.Value{}, settingError("54000", "setting result exceeds invocation byte budget")
	}
	if err := runtime.owner.syncSettingPolicy(ctx, &staged); err != nil {
		return model.Value{}, err
	}
	*runtime.owner.settings = staged
	return result, nil
}
func (runtime settingRuntime) FormatLocale(ctx context.Context) (model.FormatLocale, error) {
	runtime.owner.settingMu.Lock()
	defer runtime.owner.settingMu.Unlock()
	if err := runtime.check(ctx); err != nil {
		return model.FormatLocale{}, err
	}
	settings := runtime.owner.settings
	locale, err := settings.resolver.ResolveFormatLocale(ctx, settings.current[lcNumeric].text, settings.current[lcMonetary].text, settings.current[lcTime].text)
	if err != nil {
		return model.FormatLocale{}, err
	}
	if locale.NumericName != settings.current[lcNumeric].text || locale.MonetaryName != settings.current[lcMonetary].text || locale.TimeName != settings.current[lcTime].text {
		return model.FormatLocale{}, settingError("XX000", "format locale resolver returned a different identity")
	}
	return locale, nil
}

func (transactions *Transactions) syncSettingPolicy(ctx context.Context, settings *Settings) error {
	if !transactions.active || settings.TransactionPolicy().ReadOnly == transactions.settings.TransactionPolicy().ReadOnly {
		return nil
	}
	return transactions.transaction.SetReadOnly(ctx, settings.TransactionPolicy().ReadOnly)
}

func (runtime settingRuntime) CatalogSettings(ctx context.Context) ([]catalogrel.Setting, error) {
	runtime.owner.settingMu.Lock()
	defer runtime.owner.settingMu.Unlock()
	if err := runtime.check(ctx); err != nil {
		return nil, err
	}
	settings := runtime.owner.settings
	result := make([]catalogrel.Setting, len(settingDefinitions))
	for id, definition := range settingDefinitions {
		text, unit, valueType := settings.current[id].text, "", "string"
		reset, minimum, maximum := settings.reset[id].text, "", ""
		switch definition.kind {
		case integerSetting, memorySetting, durationSetting:
			text = strconv.FormatInt(settings.current[id].number, 10)
			reset = strconv.FormatInt(settings.reset[id].number, 10)
			minimum, maximum = strconv.FormatInt(definition.minimum, 10), strconv.FormatInt(definition.maximum, 10)
			valueType = "integer"
			if definition.kind == memorySetting {
				unit = "kB"
			}
			if definition.kind == durationSetting {
				unit = "ms"
			}
		case booleanSetting:
			valueType = "bool"
		}
		switch settingID(id) {
		case intervalStyle, byteaOutput, backslashQuote, clientMinMessages, defaultIsolation, transactionIsolation:
			valueType = "enum"
		case serverVersionNumber, maxIdentifierLength:
			valueType = "integer"
		case integerDatetimes:
			valueType = "bool"
		}
		source := "default"
		if settings.current[id].assigned {
			source = "session"
		}
		result[id] = catalogrel.Setting{Name: definition.name, Value: model.StringVal(text), ReadOnly: definition.kind == readOnlySetting, Unit: unit, Description: settingDescription(definition.name), ValueType: valueType, Source: source, MinValue: minimum, MaxValue: maximum, BootValue: reset, ResetValue: reset}
	}
	return result, nil
}

func (runtime settingRuntime) VisitSettings(ctx context.Context, visit func(model.SettingSummary) error) error {
	if visit == nil {
		return &model.Error{Code: "22023", Message: "setting visitor is nil"}
	}
	settings, err := runtime.CatalogSettings(ctx)
	if err != nil {
		return err
	}
	for _, setting := range settings {
		if err := ctx.Err(); err != nil {
			return err
		}
		summary := model.SettingSummary{
			Name:             setting.Name,
			Setting:          setting.Value.StrVal,
			Unit:             setting.Unit,
			Category:         setting.Category,
			ShortDescription: setting.Description,
			ExtraDescription: setting.ExtraDescription,
			Context:          setting.Context,
			ValueType:        setting.ValueType,
			Source:           setting.Source,
			Minimum:          setting.MinValue,
			Maximum:          setting.MaxValue,
			EnumValues:       setting.EnumValues,
			BootValue:        setting.BootValue,
			ResetValue:       setting.ResetValue,
			SourceFile:       setting.SourceFile,
			PendingRestart:   setting.PendingRestart,
			ReadOnly:         setting.ReadOnly,
		}
		if setting.SourceLine != nil {
			summary.SourceLine = *setting.SourceLine
			summary.HasSourceLine = true
		}
		if err := visit(summary); err != nil {
			return err
		}
	}
	return nil
}

var _ model.SettingsIntrospection = settingRuntime{}

func (transactions *Transactions) attachCatalogSettings(ctx context.Context, source catalogrel.Scanner) (catalogrel.Scanner, error) {
	if source == nil {
		return nil, nil
	}
	return catalogrel.AttachSettings(source, transactions.settingRuntime(ctx))
}

func attachCatalogView(source catalogrel.Scanner, view catalogrel.ReadView) (catalogrel.Scanner, error) {
	if source == nil {
		return nil, nil
	}
	return catalogrel.AttachView(source, view)
}
