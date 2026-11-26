package bind

import "github.com/gosuda/supersql-alpha/internal/zpgparse"

type returningAliases struct {
	old string
	new string
}

func bindReturningAliases(clause *zpgparse.ReturningClause, target relationQualifier, input *scope) (returningAliases, error) {
	var aliases returningAliases
	for _, node := range clause.Options {
		option := node.GetReturningOption()
		if option == nil || option.Value == "" {
			return aliases, bindError("42601", "invalid RETURNING option", 0)
		}
		position := int(option.Location) + 1
		var destination *string
		switch option.Option {
		case zpgparse.ReturningOptionKind_RETURNING_OPTION_OLD:
			destination = &aliases.old
		case zpgparse.ReturningOptionKind_RETURNING_OPTION_NEW:
			destination = &aliases.new
		default:
			return aliases, bindError("42601", "invalid RETURNING option", position)
		}
		if *destination != "" {
			return aliases, bindError("42601", "RETURNING image alias specified more than once", position)
		}
		taken, err := aliases.taken(option.Value, target, input, position)
		if err != nil {
			return aliases, err
		}
		if taken {
			return aliases, bindError("42712", "table name specified more than once", position)
		}
		*destination = option.Value
	}
	if aliases.old == "" {
		taken, err := aliases.taken("old", target, input, 0)
		if err != nil {
			return aliases, err
		}
		if !taken {
			aliases.old = "old"
		}
	}
	if aliases.new == "" {
		taken, err := aliases.taken("new", target, input, 0)
		if err != nil {
			return aliases, err
		}
		if !taken {
			aliases.new = "new"
		}
	}
	return aliases, nil
}

func (a returningAliases) taken(name string, target relationQualifier, input *scope, position int) (bool, error) {
	_, present, err := input.relationNamespace(relationQualifier{name: name}, position)
	if err != nil {
		return false, err
	}
	return present || target.name == name || a.old == name || a.new == name, nil
}
