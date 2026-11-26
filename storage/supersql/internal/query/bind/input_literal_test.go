package bind_test

import "testing"

func TestTypedLiteralErrorsPrecedeCaseBranchSelection(t *testing.T) {
	_, err := bindSQL(t, `SELECT CASE WHEN false THEN interval 'bad' ELSE interval '1 day' END`, nil, nil)
	requireCode(t, err, "22007")
}
