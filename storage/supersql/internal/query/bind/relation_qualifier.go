package bind

import "github.com/gosuda/supersql-alpha/internal/zpgparse"

// Components remain separate: a quoted identifier may itself contain dots.
type relationQualifier struct {
	namespace string
	name      string
}

func (q relationQualifier) matches(c column) bool {
	return c.qualifier == q.name && (q.namespace == "" || c.namespace == q.namespace)
}

func (q relationQualifier) present(columns []column, position int) (bool, error) {
	var namespace string
	found := false
	for _, c := range columns {
		if !q.matches(c) {
			continue
		}
		if found && namespace != c.namespace {
			return false, bindError("42P09", "table reference is ambiguous", position)
		}
		namespace, found = c.namespace, true
	}
	return found, nil
}

func setRelationNamespace(columns []column, namespace string) {
	for i := range columns {
		columns[i].namespace = namespace
	}
}

func columnsQualifier(alias *zpgparse.Alias, fallback string) string {
	if alias != nil {
		return alias.Aliasname
	}
	return fallback
}

func disjointRelations(left, right relation) error {
	seen := make(map[relationQualifier]bool, len(left.namespaces))
	names := make(map[string]bool, len(left.namespaces))
	for _, n := range left.namespaces {
		seen[n.qualifier] = true
		names[n.qualifier.name] = true
	}
	for _, n := range right.namespaces {
		q := n.qualifier
		aliasConflict := q.namespace == "" && names[q.name] || seen[relationQualifier{name: q.name}]
		if aliasConflict || seen[q] {
			return bindError("42712", "table name specified more than once", 0)
		}
	}
	return nil
}
