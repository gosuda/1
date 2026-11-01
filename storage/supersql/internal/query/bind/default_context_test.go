package bind_test

import "testing"

func TestDefaultRejectedOutsideDestinationAssignment(t *testing.T) {
	for _, sql := range []string{
		`VALUES (DEFAULT)`,
		`INSERT INTO public."Mixed" ("X") VALUES (DEFAULT) ORDER BY 1`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := bindSQL(t, sql, indexClassSnapshot(t), nil)
			requireCode(t, err, "42601")
		})
	}
}
