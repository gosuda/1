package model

// Error carries PostgreSQL SQLSTATE and an optional one-based source position.
// FeatureID identifies a product exclusion, never an implementation gap.
type Error struct {
	Code      string
	Message   string
	Detail    string
	Hint      string
	Column    string
	Constraint string
	Datatype  string
	Table     string
	Schema    string
	Context   string
	Position  int
	FeatureID string
	Cause     error
}

func (e *Error) Error() string    { return e.Message }
func (e *Error) SQLState() string { return e.Code }
func (e *Error) Unwrap() error    { return e.Cause }
