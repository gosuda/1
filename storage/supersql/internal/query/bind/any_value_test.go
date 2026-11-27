package bind_test

import "testing"

func TestAnyValueRequiresResolvedPolymorphicInput(t *testing.T) {
	for _, sql := range []string{
		`SELECT any_value(NULL)`,
		`SELECT any_value('value')`,
		`SELECT any_value($1)`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, nil, nil)
			requireCode(t, err, "42804")
			if statement != nil {
				t.Fatal("unresolved polymorphic input returned a bound statement")
			}
		})
	}
}
