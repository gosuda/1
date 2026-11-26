package fixture

// Widget is the documented exported type of the fixture package.
type Widget struct {
	// Name is the widget name.
	Name string
}

// NewWidget returns a widget with the given name.
func NewWidget(name string) *Widget { return &Widget{Name: name} }

// Render returns the widget name.
func (w *Widget) Render() string { return w.Name }

// FeatureID identifies one excluded profile feature.
type FeatureID string

// PhysicalStorage is an excluded profile feature.
const PhysicalStorage FeatureID = "physical_storage"

// Error carries a SQLSTATE like the engine's own error type.
type Error struct {
	Code    string
	Message string
}

// Error renders the SQLSTATE and message.
func (e *Error) Error() string { return e.Code + ": " + e.Message }

// bindError constructs an error from an explicit SQLSTATE.
func bindError(code, message string, position int) error {
	return &Error{Code: code, Message: message}
}

// sqlError constructs an error from an explicit SQLSTATE.
func sqlError(code, message string) error {
	return &Error{Code: code, Message: message}
}

// unsupported fixes SQLSTATE 0A000 for one profile feature, so a call site that
// passes a feature constant still resolves to a SQLSTATE.
func unsupported(feature FeatureID, position int) error {
	return &Error{Code: "0A000", Message: "feature is outside profile: " + string(feature)}
}

// guarded reports a code that is only known at run time.
func guarded(code string) error {
	return bindError(code, "dynamic code", 0)
}

// rejections exercises every SQLSTATE collection path: two literals, one of
// them malformed, one symbolic feature constant, one error composite literal,
// and one non-literal argument.
func rejections() []error {
	return []error{
		bindError("0A000", "profile rejection", 1),
		bindError("NOPE", "malformed literal", 2),
		unsupported(PhysicalStorage, 3),
		&Error{Code: "22023", Message: "invalid parameter value"},
		sqlError("23505", "duplicate key"),
		guarded("22000"),
	}
}
