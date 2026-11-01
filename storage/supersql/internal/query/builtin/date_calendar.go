package builtin

// dateCalendar follows PostgreSQL j2date's unsigned Julian-day arithmetic.
// Astronomical year zero is 1 BC; formatting/extraction applies SQL notation.
func dateCalendar(days int64) (year, month, day int64) {
	julian := uint32(days+dateEpochJulian) + uint32(32044)
	quad := julian / 146097
	extra := (julian-quad*146097)*4 + 3
	julian += 60 + quad*3 + extra/146097
	quad = julian / 1461
	julian -= quad * 1461
	year = int64(julian * 4 / 1461)
	if year != 0 {
		julian = (julian + 305) % 365
	} else {
		julian = (julian + 306) % 366
	}
	julian += 123
	year += int64(quad * 4)
	year -= 4800
	quad = julian * 2141 / 65536
	day = int64(julian - 7834*quad/256)
	month = int64((quad+10)%12 + 1)
	return
}
