// Package tzrule parses PostgreSQL 18 POSIX time zone specifications.
package tzrule

type DateKind uint8

const (
	JulianNoLeap DateKind = 1
	DayOfYear    DateKind = 2
	MonthWeekDay DateKind = 3
)

type Transition struct {
	Kind                 DateKind
	Day                  uint16
	Month, Week, Weekday uint8
	Seconds              int32
}

type Rule struct {
	StandardName, DaylightName     string
	StandardOffset, DaylightOffset int32
	Start, End                     Transition
	Daylight                       bool
}

type parseError struct{}

func (parseError) Error() string { return "invalid POSIX time zone specification" }

// Parse borrows name slices from s. Offsets are seconds east of UTC.
// PostgreSQL accepts empty standard names and arbitrary quoted characters,
// but does not accept the tzcode time-basis suffix extensions.
func Parse(s string) (Rule, error) {
	if len(s) == 0 || len(s) > 255 {
		return Rule{}, parseError{}
	}
	for i := range len(s) {
		if s[i] == 0 {
			return Rule{}, parseError{}
		}
	}
	p := parser{s: s}
	r := Rule{StandardName: p.name()}
	r.StandardOffset = -p.offset()
	if p.bad {
		return Rule{}, parseError{}
	}
	if p.i == len(s) {
		return r, nil
	}
	r.Daylight = true
	r.DaylightName = p.name()
	if r.DaylightName == "" || p.bad {
		return Rule{}, parseError{}
	}
	r.DaylightOffset = r.StandardOffset + 3600
	if p.peek() != 0 && p.peek() != ',' && p.peek() != ';' {
		r.DaylightOffset = -p.offset()
	}
	if p.bad {
		return Rule{}, parseError{}
	}
	if p.i == len(s) {
		r.Start = Transition{Kind: MonthWeekDay, Month: 3, Week: 2, Seconds: 7200}
		r.End = Transition{Kind: MonthWeekDay, Month: 11, Week: 1, Seconds: 7200}
		return r, nil
	}
	if p.peek() != ',' && p.peek() != ';' {
		return Rule{}, parseError{}
	}
	p.i++
	r.Start = p.transition()
	p.take(',')
	r.End = p.transition()
	if p.bad || p.i != len(s) {
		return Rule{}, parseError{}
	}
	return r, nil
}

type parser struct {
	s   string
	i   int
	bad bool
}

func (p *parser) peek() byte {
	if p.i >= len(p.s) {
		return 0
	}
	return p.s[p.i]
}

func (p *parser) take(c byte) {
	if p.peek() != c {
		p.bad = true
		return
	}
	p.i++
}

func (p *parser) name() string {
	if p.peek() == '<' {
		p.i++
		start := p.i
		for p.peek() != 0 && p.peek() != '>' {
			p.i++
		}
		name := p.s[start:p.i]
		p.take('>')
		return name
	}
	start := p.i
	for {
		c := p.peek()
		if c >= '0' && c <= '9' {
			break
		}
		switch c {
		case 0, ',', '-', '+':
			return p.s[start:p.i]
		}
		p.i++
	}
	return p.s[start:p.i]
}

func (p *parser) number(low, high int) int {
	c := p.peek()
	if c < '0' || c > '9' {
		p.bad = true
		return 0
	}
	n := 0
	for c >= '0' && c <= '9' {
		n = n*10 + int(c-'0')
		p.i++
		if n > high {
			p.bad = true
			return 0
		}
		c = p.peek()
	}
	if n < low {
		p.bad = true
	}
	return n
}

func (p *parser) offset() int32 {
	sign := int32(1)
	if p.peek() == '-' {
		sign = -1
		p.i++
	} else if p.peek() == '+' {
		p.i++
	}
	n := int32(p.number(0, 167)) * 3600
	if p.peek() == ':' {
		p.i++
		n += int32(p.number(0, 59)) * 60
		if p.peek() == ':' {
			p.i++
			n += int32(p.number(0, 60))
		}
	}
	return sign * n
}

func (p *parser) transition() Transition {
	r := Transition{Seconds: 7200}
	switch p.peek() {
	case 'J', 'j':
		p.i++
		r.Kind = JulianNoLeap
		r.Day = uint16(p.number(1, 365))
	case 'M', 'm':
		p.i++
		r.Kind = MonthWeekDay
		r.Month = uint8(p.number(1, 12))
		p.take('.')
		r.Week = uint8(p.number(1, 5))
		p.take('.')
		r.Weekday = uint8(p.number(0, 6))
	default:
		r.Kind = DayOfYear
		r.Day = uint16(p.number(0, 365))
	}
	if p.peek() == '/' {
		p.i++
		r.Seconds = p.offset()
	}
	return r
}
