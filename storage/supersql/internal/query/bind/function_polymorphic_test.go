package bind_test

import "testing"

func TestExactPolymorphismDoesNotPromoteDifferentArrayTypes(t *testing.T) {
	_, err := bindSQL(t, `SELECT ARRAY[1] @> ARRAY[1::bigint]`, nil, nil)
	requireCode(t, err, "42883")
}

func TestPolymorphicArrayResultRequiresAnArrayIdentity(t *testing.T) {
	_, err := bindSQL(t, `SELECT array_fill(ARRAY[1],ARRAY[2])`, nil, nil)
	requireCode(t, err, "42704")
}

func TestUnknownPolymorphicFillElementRemainsATypeError(t *testing.T) {
	_, err := bindSQL(t, `SELECT array_fill(NULL,ARRAY[2])`, nil, nil)
	requireCode(t, err, "42804")
}
