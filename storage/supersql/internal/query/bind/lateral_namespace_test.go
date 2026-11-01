package bind_test

import "testing"

func TestOuterLateralRejectsImmediateLeftStarCapture(t *testing.T) {
	for _, kind := range []string{"RIGHT", "FULL"} {
		t.Run(kind, func(t *testing.T) {
			_, err := bindSQL(t, `SELECT r.x FROM (VALUES(1)) l(x) `+kind+` JOIN LATERAL (SELECT l.*) r ON TRUE`, nil, nil)
			requireCode(t, err, "42P10")
		})
	}
}

func TestOuterLateralAllowsLocalNamespaceShadowing(t *testing.T) {
	bound, err := bindSQL(t, `SELECT r.y FROM (VALUES(1)) l(x) RIGHT JOIN LATERAL (SELECT l.y FROM (VALUES(2)) l(y)) r ON TRUE`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := bound.Columns()
	if len(columns) != 1 || columns[0].Name != "y" || columns[0].Layout.Type.ID != 23 {
		t.Fatalf("shadowed namespace result: %+v", columns)
	}
}
