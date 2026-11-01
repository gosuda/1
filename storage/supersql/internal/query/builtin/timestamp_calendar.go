package builtin

// PostgreSQL checks broad Julian month bounds before exact timestamp arithmetic.
func validTimestampCalendar(year, month int64) bool {
	afterMinimum := year > -4713 || year == -4713 && month >= 11
	beforeMaximum := year < 5874898 || year == 5874898 && month < 6
	return afterMinimum && beforeMaximum
}
