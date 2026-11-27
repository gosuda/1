package builtin

import (
	"bytes"
	"context"
	"math"
	"math/bits"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileOutput(signature Signature, options ResolveOptions) scalarKernel {
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		value := args[0]
		switch signature.Arguments[0] {
		case 20, 21, 23:
			return ownedText(ctx, budget, strconv.FormatInt(value.IntVal, 10))
		case 700, 701:
			bits := 64
			if signature.Arguments[0] == 700 {
				bits = 32
			}
			return ownedText(ctx, budget, formatFloat(value.FloatVal, bits, options.ExtraFloatDigits))
		case 16:
			if signature.Result == 25 {
				if value.IntVal != 0 {
					return ownedText(ctx, budget, "true")
				}
				return ownedText(ctx, budget, "false")
			}
			if value.IntVal != 0 {
				return ownedText(ctx, budget, "t")
			}
			return ownedText(ctx, budget, "f")
		case 17:
			return outputBytea(ctx, value.BytesVal, budget, options.ByteaEscape)
		case 25:
			if _, err := textCharacters(ctx, value.StrVal); err != nil {
				return model.Value{}, err
			}
			return ownedText(ctx, budget, value.StrVal)
		default:
			return model.Value{}, sqlError("XX000", "invalid output kernel type")
		}
	}
}

func outputBytea(ctx context.Context, input []byte, budget int64, escape bool) (model.Value, error) {
	size := int64(2) + int64(len(input))*2
	if escape {
		size = 0
		for i, b := range input {
			if i%cancellationChunk == 0 {
				if err := ctx.Err(); err != nil {
					return model.Value{}, err
				}
			}
			switch {
			case b == '\\':
				size += 2
			case b < 0x20 || b > 0x7e:
				size += 4
			default:
				size++
			}
		}
	}
	if err := outputBudget(size, budget); err != nil {
		return model.Value{}, err
	}
	var output strings.Builder
	output.Grow(int(size))
	if !escape {
		output.WriteString(`\x`)
	}
	const hex = "0123456789abcdef"
	for i, b := range input {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
		}
		switch {
		case !escape:
			output.WriteByte(hex[b>>4])
			output.WriteByte(hex[b&15])
		case b == '\\':
			output.WriteString(`\\`)
		case b < 0x20 || b > 0x7e:
			output.WriteByte('\\')
			output.WriteByte('0' + b>>6)
			output.WriteByte('0' + (b>>3)&7)
			output.WriteByte('0' + b&7)
		default:
			output.WriteByte(b)
		}
	}
	return model.StringVal(output.String()), nil
}

func formatFloat(value float64, width, extraDigits int) string {
	if literal := floatOutputLiteral(value, extraDigits); literal != "" {
		return literal
	}
	var buffer [32]byte
	return string(appendFiniteFloat(buffer[:0], value, width, extraDigits))
}

func floatOutputLiteral(value float64, extraDigits int) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	case value == 0 && extraDigits > 0:
		if math.Signbit(value) {
			return "-0"
		}
		return "0"
	default:
		return ""
	}
}

func appendFloatOutput(output []byte, value float64, width, extraDigits int) []byte {
	if literal := floatOutputLiteral(value, extraDigits); literal != "" {
		return append(output, literal...)
	}
	return appendFiniteFloat(output, value, width, extraDigits)
}

func appendFiniteFloat(output []byte, value float64, width, extraDigits int) []byte {
	if extraDigits <= 0 {
		precision := 15 + extraDigits
		if width == 32 {
			precision = 6 + extraDigits
		}
		return strconv.AppendFloat(output, value, 'g', max(1, precision), width)
	}
	if math.Signbit(value) {
		output = append(output, '-')
	}
	value = math.Abs(value)
	var buffer [32]byte
	decimal := strconv.AppendFloat(buffer[:0], value, 'e', -1, width)
	mantissa, exponentText, _ := bytes.Cut(decimal, []byte("e"))
	exponent := floatOutputExponent(exponentText)
	var coefficient uint64
	digits := 0
	for _, digit := range mantissa {
		if digit != '.' {
			coefficient = coefficient*10 + uint64(digit-'0')
			digits++
		}
	}
	// PG18.4 Ryu excludes exact binary midpoints from its rounding interval.
	if decimalAtMidpoint(value, width, coefficient, exponent-digits+1) {
		decimal = strconv.AppendFloat(buffer[:0], value, 'e', digits, width)
		mantissa, exponentText, _ = bytes.Cut(decimal, []byte("e"))
		mantissa = bytes.TrimRight(bytes.TrimRight(mantissa, "0"), ".")
		exponent = floatOutputExponent(exponentText)
	}
	threshold := 15
	if width == 32 {
		threshold = 6
	}
	if exponent >= -4 && exponent < threshold {
		return appendFloatFixed(output, mantissa, exponent+1)
	}
	output = append(output, mantissa...)
	output = append(output, 'e')
	return append(output, exponentText...)
}

func floatOutputExponent(text []byte) int {
	value := 0
	for _, digit := range text[1:] {
		value = value*10 + int(digit-'0')
	}
	if text[0] == '-' {
		return -value
	}
	return value
}

func appendFloatFixed(output, mantissa []byte, point int) []byte {
	if point <= 0 {
		output = append(output, '0', '.')
		for range -point {
			output = append(output, '0')
		}
	}
	digits := 0
	for _, digit := range mantissa {
		if digit == '.' {
			continue
		}
		if point > 0 && digits == point {
			output = append(output, '.')
		}
		output = append(output, digit)
		digits++
	}
	for ; digits < point; digits++ {
		output = append(output, '0')
	}
	return output
}

func decimalAtMidpoint(value float64, width int, coefficient uint64, exponent int) bool {
	// Normalize the decimal to an odd integer times a power of two. A
	// remaining factor of five in the denominator cannot be a midpoint.
	zeros := bits.TrailingZeros64(coefficient)
	coefficient >>= zeros
	power := exponent + zeros
	if exponent < 0 {
		for range -exponent {
			if coefficient%5 != 0 {
				return false
			}
			coefficient /= 5
		}
	} else {
		for range exponent {
			if coefficient > math.MaxUint64/5 {
				return false
			}
			coefficient *= 5
		}
	}
	encoded := math.Float64bits(value)
	mantissa := encoded & ((1 << 52) - 1)
	binaryExponent := int(encoded >> 52)
	fractionBits, bias := 52, 1023
	if width == 32 {
		encoded = uint64(math.Float32bits(float32(value)))
		mantissa = encoded & ((1 << 23) - 1)
		binaryExponent = int(encoded >> 23)
		fractionBits, bias = 23, 127
	}
	if binaryExponent != 0 {
		mantissa |= 1 << fractionBits
	}
	binaryPower := max(binaryExponent, 1) - bias - fractionBits
	if coefficient == 2*mantissa+1 && power == binaryPower-1 {
		return true
	}
	if binaryExponent > 1 && mantissa == 1<<fractionBits {
		return coefficient == 4*mantissa-1 && power == binaryPower-2
	}
	return coefficient == 2*mantissa-1 && power == binaryPower-1
}
