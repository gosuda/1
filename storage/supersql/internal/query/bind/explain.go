package bind

import (
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type ExplainFormat uint8

const (
	ExplainText ExplainFormat = iota
	ExplainXML
	ExplainJSON
	ExplainYAML
)

type ExplainSerialize uint8

const (
	ExplainNoSerialization ExplainSerialize = iota
	ExplainSerializeText
	ExplainSerializeBinary
)

type ExplainOptions struct {
	Analyze     bool
	Verbose     bool
	Costs       bool
	Settings    bool
	Timing      bool
	Summary     bool
	GenericPlan bool
	Format      ExplainFormat
	Serialize   ExplainSerialize
}

type ExplainCommand struct {
	Query   *zpgparse.Node
	Options ExplainOptions
}

func (ExplainCommand) boundCommand() {}

func bindExplain(statement *zpgparse.ExplainStmt, textLayout ResultColumn) (ExplainCommand, []ResultColumn, error) {
	command := ExplainCommand{Options: ExplainOptions{Costs: true, Format: ExplainText}}
	if statement == nil || statement.Query == nil {
		return command, nil, bindError("42601", "EXPLAIN requires a statement", 0)
	}
	seen := make(map[string]bool, len(statement.Options))
	timingSet, summarySet, serializeSet := false, false, false
	// buffers, wal and memory parse like every other option but are rejected
	// after the option-consistency checks: the embedded profile has no server
	// instrumentation to report them from.
	buffers, wal, memory := false, false, false
	for _, node := range statement.Options {
		option := node.GetDefElem()
		if option == nil || option.Defnamespace != "" || option.Defaction != zpgparse.DefElemAction_DEFELEM_UNSPEC {
			return command, nil, bindError("42601", "invalid EXPLAIN option", 0)
		}
		name := strings.ToLower(option.Defname)
		position := int(option.Location) + 1
		if seen[name] {
			return command, nil, bindError("42601", "EXPLAIN option specified more than once: "+name, position)
		}
		seen[name] = true
		var err error
		switch name {
		case "analyze":
			command.Options.Analyze, err = explainBooleanOption(option)
		case "verbose":
			command.Options.Verbose, err = explainBooleanOption(option)
		case "costs":
			command.Options.Costs, err = explainBooleanOption(option)
		case "settings":
			command.Options.Settings, err = explainBooleanOption(option)
		case "buffers":
			buffers, err = explainBooleanOption(option)
		case "wal":
			wal, err = explainBooleanOption(option)
		case "timing":
			timingSet = true
			command.Options.Timing, err = explainBooleanOption(option)
		case "summary":
			summarySet = true
			command.Options.Summary, err = explainBooleanOption(option)
		case "memory":
			memory, err = explainBooleanOption(option)
		case "generic_plan":
			command.Options.GenericPlan, err = explainBooleanOption(option)
		case "format":
			command.Options.Format, err = explainFormatOption(option)
		case "serialize":
			serializeSet = true
			command.Options.Serialize, err = explainSerializeOption(option)
		default:
			return command, nil, bindError("42601", "unrecognized EXPLAIN option: "+name, position)
		}
		if err != nil {
			return command, nil, err
		}
	}
	if !timingSet {
		command.Options.Timing = command.Options.Analyze
	}
	if !summarySet {
		command.Options.Summary = command.Options.Analyze
	}
	if command.Options.Timing && !command.Options.Analyze {
		return command, nil, bindError("22023", "EXPLAIN TIMING requires ANALYZE", 0)
	}
	if buffers && !command.Options.Analyze {
		return command, nil, bindError("22023", "EXPLAIN BUFFERS requires ANALYZE", 0)
	}
	if wal && !command.Options.Analyze {
		return command, nil, bindError("22023", "EXPLAIN WAL requires ANALYZE", 0)
	}
	if serializeSet && !command.Options.Analyze {
		return command, nil, bindError("22023", "EXPLAIN SERIALIZE requires ANALYZE", 0)
	}
	if command.Options.GenericPlan && command.Options.Analyze {
		return command, nil, bindError("22023", "EXPLAIN GENERIC_PLAN cannot be used with ANALYZE", 0)
	}
	if buffers || wal || memory {
		return command, nil, unsupported(ServerObservability, 0)
	}
	command.Query = proto.Clone(statement.Query).(*zpgparse.Node)
	return command, []ResultColumn{textLayout}, nil
}

func explainBooleanOption(option *zpgparse.DefElem) (bool, error) {
	if option.Arg == nil {
		return true, nil
	}
	value, ok := copyRawString(option.Arg)
	if !ok {
		return false, bindError("22023", "EXPLAIN option requires a boolean", int(option.Location)+1)
	}
	switch strings.ToLower(value) {
	case "true", "on", "yes", "1":
		return true, nil
	case "false", "off", "no", "0":
		return false, nil
	default:
		return false, bindError("22023", "EXPLAIN option requires a boolean", int(option.Location)+1)
	}
}

func explainFormatOption(option *zpgparse.DefElem) (ExplainFormat, error) {
	if option.Arg == nil {
		return ExplainText, bindError("22023", "EXPLAIN FORMAT requires a value", int(option.Location)+1)
	}
	value, ok := copyRawString(option.Arg)
	if !ok {
		return ExplainText, bindError("22023", "EXPLAIN FORMAT requires a value", int(option.Location)+1)
	}
	switch strings.ToLower(value) {
	case "text":
		return ExplainText, nil
	case "xml":
		return ExplainXML, nil
	case "json":
		return ExplainJSON, nil
	case "yaml":
		return ExplainYAML, nil
	default:
		return ExplainText, bindError("22023", "unrecognized EXPLAIN format", int(option.Location)+1)
	}
}

func explainSerializeOption(option *zpgparse.DefElem) (ExplainSerialize, error) {
	if option.Arg == nil {
		return ExplainSerializeText, nil
	}
	value, ok := copyRawString(option.Arg)
	if !ok {
		return ExplainNoSerialization, bindError("22023", "EXPLAIN SERIALIZE requires NONE, TEXT, or BINARY", int(option.Location)+1)
	}
	switch strings.ToLower(value) {
	case "none":
		return ExplainNoSerialization, nil
	case "text":
		return ExplainSerializeText, nil
	case "binary":
		return ExplainSerializeBinary, nil
	default:
		return ExplainNoSerialization, bindError("22023", "EXPLAIN SERIALIZE requires NONE, TEXT, or BINARY", int(option.Location)+1)
	}
}
