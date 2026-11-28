package builtin

import "math"

// Below this reduced magnitude, range-reduction error dominates the sine's
// low bits; elsewhere retain math's tuned full-argument polynomial path.
const floatTrigCancellationRange = 0x1p-26

// Small-angle reduction retains the low half-pi bits through cancellation near
// an axis. Larger arguments keep math's full-range reduction.
func floatTrigReducedAngle(angle float64) (float64, int) {
	const halfPiTail = 6.12323399573676603587e-17
	quadrant := int(math.Round(angle / (math.Pi / 2)))
	reduced := math.FMA(-float64(quadrant), math.Pi/2, angle) - float64(quadrant)*halfPiTail
	return reduced, quadrant & 3
}

func floatTrigSin(angle float64) float64 {
	if math.Abs(angle) > 2*math.Pi {
		return math.Sin(angle)
	}
	reduced, quadrant := floatTrigReducedAngle(angle)
	if math.Abs(reduced) >= floatTrigCancellationRange {
		return math.Sin(angle)
	}
	var result float64
	if quadrant&1 == 0 {
		result = math.Sin(reduced)
	} else {
		result = math.Cos(reduced)
	}
	if quadrant&2 != 0 {
		return -result
	}
	return result
}

func floatTrigCos(angle float64) float64 {
	if math.Abs(angle) > 2*math.Pi {
		return math.Cos(angle)
	}
	reduced, quadrant := floatTrigReducedAngle(angle)
	if math.Abs(reduced) >= floatTrigCancellationRange {
		return math.Cos(angle)
	}
	var result float64
	if quadrant&1 == 0 {
		result = math.Cos(reduced)
	} else {
		result = math.Sin(reduced)
	}
	if quadrant == 1 || quadrant == 2 {
		return -result
	}
	return result
}
