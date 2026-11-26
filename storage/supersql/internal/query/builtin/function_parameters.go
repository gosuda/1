package builtin

import (
	"sort"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type functionParameterInfo struct {
	ID                  model.FunctionID
	Names, Defaults     uint32
	Count, DefaultCount uint8
	AllTypes            [MaxCallArguments]uint32
	AllModes            [MaxCallArguments]byte
	AllNames            [MaxCallArguments]string
	AllCount            uint8
	AllTypesPresent     bool
	AllModesPresent     bool
	AllNamesPresent     bool
}

// FunctionParameters is an immutable view of input names and trailing defaults.
// DefaultSQL is scalar SQL from the pinned bootstrap declarations, not executable support.
type FunctionParameters struct{ entry *functionParameterInfo }

func LookupFunctionParameters(id model.FunctionID) (FunctionParameters, bool) {
	index := sort.Search(len(functionParameterMetadata), func(i int) bool { return functionParameterMetadata[i].ID >= id })
	if index == len(functionParameterMetadata) || functionParameterMetadata[index].ID != id {
		return FunctionParameters{}, false
	}
	return FunctionParameters{entry: &functionParameterMetadata[index]}, true
}

func (parameters FunctionParameters) ArgumentCount() int {
	if parameters.entry == nil {
		return 0
	}
	return int(parameters.entry.Count)
}

func (parameters FunctionParameters) RequiredCount() int {
	if parameters.entry == nil {
		return 0
	}
	return int(parameters.entry.Count - parameters.entry.DefaultCount)
}

func (parameters FunctionParameters) ArgumentName(index int) (string, bool) {
	if index < 0 || index >= parameters.ArgumentCount() || parameters.entry.Names == 0 {
		return "", false
	}
	name := functionParameterNames[int(parameters.entry.Names)+index]
	return name, name != ""
}

func (parameters FunctionParameters) DefaultSQL(index int) (string, bool) {
	if index < parameters.RequiredCount() || index >= parameters.ArgumentCount() {
		return "", false
	}
	return functionParameterDefaults[int(parameters.entry.Defaults)+index-parameters.RequiredCount()], true
}

func (parameters FunctionParameters) AllArgumentCount() int {
	if parameters.entry == nil {
		return 0
	}
	return int(parameters.entry.AllCount)
}

func (parameters FunctionParameters) AllArgument(index int) (model.TypeID, byte, string, bool) {
	if parameters.entry == nil || index < 0 || index >= int(parameters.entry.AllCount) {
		return 0, 0, "", false
	}
	return model.TypeID(parameters.entry.AllTypes[index]), parameters.entry.AllModes[index], parameters.entry.AllNames[index], true
}
func (parameters FunctionParameters) HasAllArgumentTypes() bool {
	return parameters.entry != nil && parameters.entry.AllTypesPresent
}

func (parameters FunctionParameters) HasAllArgumentModes() bool {
	return parameters.entry != nil && parameters.entry.AllModesPresent
}


func (parameters FunctionParameters) HasAllArgumentNames() bool {
	return parameters.entry != nil && parameters.entry.AllNamesPresent
}
