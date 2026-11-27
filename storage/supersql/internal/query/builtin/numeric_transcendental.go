package builtin

import (
	"context"
	"errors"
	"math"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/decimalint"
)

// PostgreSQL 18.4 uses binary floating point only to select display and working
// scales. All result digits below are computed with owned decimal coefficients.
func numericScaleEstimate(ctx context.Context, value decimal) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if value.coefficient.Sign() == 0 {
		return 0, nil
	}
	// Every binary64 rounding boundary has a terminating decimal expansion
	// shorter than 1100 significant digits. Preserve a sticky digit beyond
	// that boundary instead of allocating the entire numeric text.
	var buffer [1120]byte
	text := buffer[:0]
	if value.coefficient.Sign() < 0 {
		text = append(text, '-')
	}
	digits := numericDigits(value.coefficient)
	count := min(digits, 1100)
	for i := range count {
		text = append(text, '0'+value.coefficient.DecimalDigit(i))
		if i == 0 {
			text = append(text, '.')
		}
	}
	for i := count; i < digits; i++ {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if value.coefficient.DecimalDigit(i) != 0 {
			text = append(text, '1')
			break
		}
	}
	text = append(text, 'e')
	text = strconv.AppendInt(text, int64(digits-value.scale-1), 10)
	estimate, err := strconv.ParseFloat(string(text), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, err
	}
	return estimate, ctx.Err()
}

func numericDisplayScale(weight int, scales ...int) int {
	scale := max(16-weight, 0)
	for _, input := range scales {
		scale = max(scale, input)
	}
	return min(scale, 1000)
}

// A calculation owns its intermediate slots. Replacing a slot releases its old
// coefficient immediately; the enclosing checkpoint releases constants and
// short-lived expression results on every exit, including cancellation.
type numericCalculation struct {
	ctx  context.Context
	work *decimalint.Workspace
	err  error
}

func (c *numericCalculation) integer(value int64) decimal {
	if c.err != nil {
		return decimal{}
	}
	coefficient, err := c.work.FromInt64(c.ctx, value)
	c.err = err
	return decimal{coefficient: coefficient}
}

func (c *numericCalculation) clone(value decimal) decimal {
	if c.err != nil {
		return decimal{}
	}
	if value.special != numericFinite {
		return value
	}
	coefficient, err := c.work.Clone(c.ctx, value.coefficient)
	c.err = err
	return decimal{coefficient: coefficient, scale: value.scale}
}

func (c *numericCalculation) replace(slot *decimal, value decimal) {
	if slot.coefficient != value.coefficient {
		c.err = errors.Join(c.err, slot.coefficient.Close())
	}
	*slot = value
}

func (c *numericCalculation) compare(left, right decimal) int {
	if c.err != nil {
		return 0
	}
	order, err := numericCompare(c.ctx, left, right)
	c.err = err
	return order
}

func (c *numericCalculation) zero(value decimal) bool {
	return c.err != nil || value.special == numericFinite && value.coefficient.Sign() == 0
}

func (c *numericCalculation) round(value decimal, scale int) decimal {
	if c.err != nil {
		return decimal{}
	}
	result, err := numericRescale(c.ctx, c.work, value, scale, true)
	c.err = err
	return result
}

func (c *numericCalculation) arithmetic(left, right decimal, op primitiveOp) decimal {
	if c.err != nil {
		return decimal{}
	}
	result, err := numericArithmetic(c.ctx, c.work, left, right, op)
	c.err = err
	return result
}

func (c *numericCalculation) multiply(left, right decimal, scale int) (result decimal) {
	if c.err != nil {
		return decimal{}
	}
	checkpoint := c.work.Checkpoint()
	defer func() { c.err = errors.Join(c.err, c.work.ReleaseAfter(checkpoint, result.coefficient)) }()
	if c.err = numericWork(c.ctx, numericDigits(left.coefficient)+numericDigits(right.coefficient)); c.err != nil {
		return decimal{}
	}
	coefficient, err := c.work.Mul(c.ctx, left.coefficient, right.coefficient)
	c.err = err
	if err != nil {
		return decimal{}
	}
	return c.round(decimal{coefficient: coefficient, scale: left.scale + right.scale}, scale)
}

func (c *numericCalculation) divide(left, right decimal, scale int) (result decimal) {
	if c.err != nil {
		return decimal{}
	}
	checkpoint := c.work.Checkpoint()
	defer func() { c.err = errors.Join(c.err, c.work.ReleaseAfter(checkpoint, result.coefficient)) }()
	if right.coefficient.Sign() == 0 {
		c.err = sqlError("22012", "division by zero")
		return decimal{}
	}
	shift := right.scale + scale - left.scale
	numerator, denominator := left.coefficient, right.coefficient
	power, err := numericPower(c.ctx, c.work, max(shift, -shift))
	c.err = err
	if err != nil {
		return decimal{}
	}
	if shift >= 0 {
		if c.err = numericWork(c.ctx, numerator.Digits()+shift); c.err == nil {
			numerator, c.err = c.work.Mul(c.ctx, numerator, power)
		}
	} else {
		if c.err = numericWork(c.ctx, denominator.Digits()-shift); c.err == nil {
			denominator, c.err = c.work.Mul(c.ctx, denominator, power)
		}
	}
	if c.err != nil {
		return decimal{}
	}
	quotient, remainder, err := c.work.QuoRem(c.ctx, numerator, denominator)
	c.err = err
	if err != nil {
		return decimal{}
	}
	up, err := numericRoundHalf(c.ctx, c.work, remainder, denominator)
	c.err = err
	if err != nil {
		return decimal{}
	}
	if up {
		quotient, c.err = numericIncrement(c.ctx, c.work, quotient, left.coefficient.Sign()*right.coefficient.Sign())
	}
	if c.err != nil {
		return decimal{}
	}
	return decimal{coefficient: quotient, scale: scale}
}

func (c *numericCalculation) sqrt(value decimal, scale int) (result decimal) {
	if c.err != nil {
		return decimal{}
	}
	checkpoint := c.work.Checkpoint()
	defer func() { c.err = errors.Join(c.err, c.work.ReleaseAfter(checkpoint, result.coefficient)) }()
	if scale < 0 {
		value.scale -= 2 * scale
	}
	root, err := numericSqrt(c.ctx, c.work, value, max(scale, 0))
	c.err = err
	if err != nil {
		return decimal{}
	}
	if scale < 0 {
		root.scale = scale
		return c.round(root, 0)
	}
	return root
}

func numericSignificantDigits(ctx context.Context, value decimal) (int, error) {
	digits := numericDigits(value.coefficient)
	for digits > 0 && value.coefficient.DecimalDigit(digits-1) == 0 {
		if digits&255 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		digits--
	}
	return digits, ctx.Err()
}

func numericExp(ctx context.Context, work *decimalint.Workspace, value decimal) (decimal, error) {
	if value.special == numericNegativeInfinity {
		coefficient, err := work.FromInt64(ctx, 0)
		return decimal{coefficient: coefficient}, err
	}
	if value.special != numericFinite {
		return value, ctx.Err()
	}
	estimate, err := numericScaleEstimate(ctx, value)
	if err != nil {
		return decimal{}, err
	}
	estimate = max(-2000.0, min(2000.0, estimate*0.434294481903252))
	return numericExpAtScale(ctx, work, value, numericDisplayScale(int(estimate), value.scale))
}

func numericExpAtScale(ctx context.Context, work *decimalint.Workspace, value decimal, scale int) (result decimal, err error) {
	checkpoint := work.Checkpoint()
	defer func() { err = errors.Join(err, work.ReleaseAfter(checkpoint, result.coefficient)) }()
	c := numericCalculation{ctx: ctx, work: work}
	estimate, err := numericScaleEstimate(ctx, value)
	if err != nil {
		return decimal{}, err
	}
	if math.Abs(estimate) >= 6000 {
		if estimate > 0 {
			return decimal{}, numericOverflow()
		}
		result = c.integer(0)
		result.scale = scale
		return result, c.err
	}
	weight := int(estimate * 0.434294481903252)
	x := c.clone(value)
	divisions := 0
	for math.Abs(estimate) > 0.01 {
		estimate /= 2
		divisions++
	}
	if divisions != 0 {
		c.replace(&x, c.divide(x, c.integer(1<<divisions), x.scale+divisions))
	}
	significant := max(1+weight+scale+int(float64(divisions)*0.301029995663981), 0) + 8
	localScale := significant - 1
	result = c.arithmetic(c.integer(1), x, opPl)
	term := c.multiply(x, x, localScale)
	c.replace(&term, c.divide(term, c.integer(2), localScale))
	for n := int64(3); !c.zero(term); n++ {
		if n > numericWorkDigits*2 {
			return decimal{}, numericResourceError()
		}
		iteration := work.Checkpoint()
		c.replace(&result, c.arithmetic(result, term, opPl))
		c.replace(&term, c.multiply(term, x, localScale))
		c.replace(&term, c.divide(term, c.integer(n), localScale))
		c.err = errors.Join(c.err, work.ReleaseAfter(iteration, result.coefficient, term.coefficient))
	}
	for ; divisions > 0 && c.err == nil; divisions-- {
		resultWeight, _ := numericLeading(result)
		localScale = max(significant-resultWeight*8, 0)
		c.replace(&result, c.multiply(result, result, localScale))
	}
	c.replace(&result, c.round(result, scale))
	return result, c.err
}

func numericLnWeight(ctx context.Context, work *decimalint.Workspace, value decimal) (weight int, err error) {
	if numericSign(value) <= 0 || value.special != numericFinite {
		return 0, ctx.Err()
	}
	checkpoint := work.Checkpoint()
	defer func() { err = errors.Join(err, work.ReleaseAfter(checkpoint)) }()
	c := numericCalculation{ctx: ctx, work: work}
	low, high := c.integer(9), c.integer(11)
	low.scale, high.scale = 1, 1
	if c.compare(value, low) >= 0 && c.compare(value, high) <= 0 {
		difference := c.arithmetic(value, c.integer(1), opMi)
		if !c.zero(difference) {
			weight = numericDigits(difference.coefficient) - difference.scale - 1
		}
		return weight, c.err
	}
	if c.err != nil {
		return 0, c.err
	}
	baseWeight, first := numericLeading(value)
	if first == 0 {
		return 0, nil
	}
	digits := numericDigits(value.coefficient)
	width := digits - value.scale - baseWeight*4
	last, err := numericSignificantDigits(ctx, value)
	if err != nil {
		return 0, err
	}
	leading, decimalWeight := first, baseWeight*4
	if last > width {
		for i := range 4 {
			leading = leading*10 + int(value.coefficient.DecimalDigit(width+i))
		}
		decimalWeight -= 4
	}
	logarithm := math.Log(float64(leading)) + float64(decimalWeight)*2.302585092994046
	return int(math.Log10(math.Abs(logarithm))), ctx.Err()
}

func numericLogDomain(value decimal) error {
	if numericSign(value) < 0 {
		return sqlError("2201E", "cannot take logarithm of a negative number")
	}
	if numericSign(value) == 0 {
		return sqlError("2201E", "cannot take logarithm of zero")
	}
	return nil
}

func numericLn(ctx context.Context, work *decimalint.Workspace, value decimal) (decimal, error) {
	if value.special == numericNaN || value.special == numericPositiveInfinity {
		return value, ctx.Err()
	}
	if err := numericLogDomain(value); err != nil {
		return decimal{}, err
	}
	weight, err := numericLnWeight(ctx, work, value)
	if err != nil {
		return decimal{}, err
	}
	return numericLnAtScale(ctx, work, value, numericDisplayScale(weight, value.scale))
}

func numericLnAtScale(ctx context.Context, work *decimalint.Workspace, value decimal, scale int) (result decimal, err error) {
	if err = numericLogDomain(value); err != nil {
		return decimal{}, err
	}
	checkpoint := work.Checkpoint()
	defer func() { err = errors.Join(err, work.ReleaseAfter(checkpoint, result.coefficient)) }()
	c := numericCalculation{ctx: ctx, work: work}
	one, two := c.integer(1), c.integer(2)
	low, high := c.integer(9), c.integer(11)
	low.scale, high.scale = 1, 1
	x, factor := c.clone(value), c.clone(two)
	roots := 0
	for c.err == nil && (c.compare(x, low) <= 0 || c.compare(x, high) >= 0) {
		if roots >= 64 {
			return decimal{}, numericResourceError()
		}
		weight, _ := numericLeading(x)
		c.replace(&x, c.sqrt(x, scale-weight*2+8))
		c.replace(&factor, c.multiply(factor, two, 0))
		roots++
	}
	localScale := scale + int(float64(roots+1)*0.301029995663981) + 8
	result = c.arithmetic(x, one, opMi)
	denominator := c.arithmetic(x, one, opPl)
	c.replace(&result, c.divide(result, denominator, localScale))
	power := c.clone(result)
	c.replace(&x, c.multiply(result, result, localScale))
	for n := int64(3); c.err == nil; n += 2 {
		if n > numericWorkDigits*2 {
			return decimal{}, numericResourceError()
		}
		iteration := work.Checkpoint()
		c.replace(&power, c.multiply(power, x, localScale))
		term := c.divide(power, c.integer(n), localScale)
		if c.zero(term) {
			c.err = errors.Join(c.err, work.ReleaseAfter(iteration, power.coefficient))
			break
		}
		c.replace(&result, c.arithmetic(result, term, opPl))
		if c.err != nil {
			break
		}
		termWeight, _ := numericLeading(term)
		resultWeight, _ := numericLeading(result)
		c.err = errors.Join(c.err, work.ReleaseAfter(iteration, power.coefficient, result.coefficient))
		if termWeight < resultWeight-localScale/2 {
			break
		}
	}
	c.replace(&result, c.multiply(result, factor, scale))
	return result, c.err
}

func numericLog(ctx context.Context, work *decimalint.Workspace, base, value decimal) (result decimal, err error) {
	if base.special == numericNaN || value.special == numericNaN {
		return decimal{special: numericNaN}, ctx.Err()
	}
	if err = numericLogDomain(base); err != nil {
		return decimal{}, err
	}
	if err = numericLogDomain(value); err != nil {
		return decimal{}, err
	}
	if base.special == numericPositiveInfinity {
		if value.special == numericPositiveInfinity {
			return decimal{special: numericNaN}, ctx.Err()
		}
		coefficient, err := work.FromInt64(ctx, 0)
		return decimal{coefficient: coefficient}, err
	}
	if value.special == numericPositiveInfinity {
		return value, ctx.Err()
	}
	checkpoint := work.Checkpoint()
	defer func() { err = errors.Join(err, work.ReleaseAfter(checkpoint, result.coefficient)) }()
	baseWeight, err := numericLnWeight(ctx, work, base)
	if err != nil {
		return decimal{}, err
	}
	valueWeight, err := numericLnWeight(ctx, work, value)
	if err != nil {
		return decimal{}, err
	}
	weight := valueWeight - baseWeight
	scale := numericDisplayScale(weight, base.scale, value.scale)
	baseLog, err := numericLnAtScale(ctx, work, base, max(scale+weight-baseWeight+8, 0))
	if err != nil {
		return decimal{}, err
	}
	valueLog, err := numericLnAtScale(ctx, work, value, max(scale+weight-valueWeight+8, 0))
	if err != nil {
		return decimal{}, err
	}
	c := numericCalculation{ctx: ctx, work: work}
	result = c.divide(valueLog, baseLog, scale)
	return result, c.err
}
