package builtin

// DateStyle controls SQL date output independently of parsing field order.
type DateStyle uint8

const (
	DateISO DateStyle = iota
	DateSQL
	DatePostgres
	DateGerman
)

type DateOrder uint8

const (
	DateMDY DateOrder = iota
	DateDMY
	DateYMD
)

func (style DateStyle) valid() bool { return style <= DateGerman }
func (order DateOrder) valid() bool { return order <= DateYMD }
