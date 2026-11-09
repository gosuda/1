package model

// Principal is the immutable logical owner, not an authorization capability.
type Principal struct{}

func (Principal) Name() string { return "embedded" }
func (Principal) OID() OID     { return 10 }
