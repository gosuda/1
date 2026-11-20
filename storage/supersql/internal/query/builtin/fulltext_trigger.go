package builtin

import (
	"context"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// FullTextTriggerContext contains the immutable table layout and one borrowed
// NEW row for a native tsvector update trigger invocation.
type FullTextTriggerContext struct {
	Columns    []model.ColumnDescriptor
	Arguments  []string
	Event      model.TriggerEvent
	New        *model.Row
	Settings   model.SettingsRuntime
	TextSearch model.TextSearchRuntime
	MaxBytes   uint64
}

func InvokeFullTextTrigger(ctx context.Context, id model.FunctionID, trigger FullTextTriggerContext) (*model.Row, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if id != 3752 && id != 3753 {
		return nil, false, &UnknownFunctionError{ID: id}
	}
	if trigger.Event != model.TriggerInsert && trigger.Event != model.TriggerUpdate {
		return nil, false, sqlError("09000", "tsvector update trigger fired for an unsupported event")
	}
	if trigger.New == nil || len(trigger.Arguments) < 3 {
		return nil, false, sqlError("09000", "tsvector update trigger requires target, configuration, and source columns")
	}
	target := fullTextTriggerColumn(trigger.Columns, trigger.Arguments[0])
	if target < 0 || trigger.Columns[target].Layout.Type.ID != 3614 || target >= len(trigger.New.Values) {
		return nil, false, sqlError("42703", "tsvector update trigger target column does not exist or is not tsvector")
	}
	budget := CallBudget{OutputBytes: int64(min(trigger.MaxBytes, uint64(^uint64(0)>>1))), MemoryBytes: trigger.MaxBytes, Settings: trigger.Settings, TextSearch: trigger.TextSearch}
	configuration, err := fullTextTriggerConfiguration(ctx, id, trigger, budget)
	if err != nil {
		return nil, false, err
	}
	var document strings.Builder
	for _, name := range trigger.Arguments[2:] {
		column := fullTextTriggerColumn(trigger.Columns, name)
		if column < 0 || column >= len(trigger.New.Values) || trigger.Columns[column].Layout.Type.Storage != model.TypeString {
			return nil, false, sqlError("42703", "tsvector update trigger source column does not exist or is not textual")
		}
		value := trigger.New.Values[column]
		if value.IsNull {
			continue
		}
		if document.Len() != 0 {
			document.WriteByte(' ')
		}
		if uint64(len(value.StrVal)) > trigger.MaxBytes || uint64(document.Len()) > trigger.MaxBytes-uint64(len(value.StrVal)) {
			return nil, false, sqlError("54000", "tsvector update trigger input exceeded byte budget")
		}
		document.WriteString(value.StrVal)
	}
	vector, err := toTSVector(ctx, configuration, document.String(), int64(min(trigger.MaxBytes, uint64(^uint64(0)>>1))))
	if err != nil {
		return nil, false, err
	}
	row, err := copyFullTextTriggerRow(ctx, trigger.New, target, vector, trigger.MaxBytes)
	if err != nil {
		return nil, false, err
	}
	return row, true, nil
}

func fullTextTriggerColumn(columns []model.ColumnDescriptor, name string) int {
	for i, column := range columns {
		if column.Name == name {
			return i
		}
	}
	return -1
}

func fullTextTriggerConfiguration(ctx context.Context, id model.FunctionID, trigger FullTextTriggerContext, budget CallBudget) (model.TextSearchConfiguration, error) {
	if id == 3752 {
		if configuration, ok := builtinTextSearchConfigurationByName(trigger.Arguments[1]); ok {
			return configuration, nil
		}
		if trigger.TextSearch == nil {
			return model.TextSearchConfiguration{}, sqlError("42704", "text search configuration does not exist")
		}
		return trigger.TextSearch.ResolveTextSearchConfiguration(ctx, trigger.Arguments[1])
	}
	column := fullTextTriggerColumn(trigger.Columns, trigger.Arguments[1])
	if column < 0 || column >= len(trigger.New.Values) || trigger.Columns[column].Layout.Type.ID != 3734 {
		return model.TextSearchConfiguration{}, sqlError("42703", "tsvector update trigger configuration column does not exist or is not regconfig")
	}
	value := trigger.New.Values[column]
	if value.IsNull {
		return model.TextSearchConfiguration{}, sqlError("23502", "tsvector update trigger configuration column must not be null")
	}
	return resolveTextSearchConfiguration(ctx, &value, budget)
}

func copyFullTextTriggerRow(ctx context.Context, source *model.Row, target int, vector []byte, maximum uint64) (*model.Row, error) {
	fixed := uint64(len(source.Key)) + uint64(len(source.Values))*uint64(unsafe.Sizeof(model.Value{})) + uint64(len(vector))
	if fixed > maximum {
		return nil, sqlError("54000", "tsvector update trigger row exceeded byte budget")
	}
	remaining := maximum - fixed
	row := *source
	row.Key = append([]byte(nil), source.Key...)
	row.Values = make([]model.Value, len(source.Values))
	row.Values[target] = model.BytesVal(vector)
	for i, value := range source.Values {
		if i == target {
			continue
		}
		owned, used, err := model.CopyValue(ctx, value, remaining)
		if err != nil {
			return nil, err
		}
		row.Values[i] = owned
		remaining -= used
	}
	return &row, nil
}
