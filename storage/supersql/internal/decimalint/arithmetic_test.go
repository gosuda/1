package decimalint

import (
	"context"
	"errors"
	"math/big"
	"math/rand/v2"
	"strings"
	"testing"
	"unsafe"
)

func arithmeticWorkspace(t *testing.T, limit uint64) *Workspace {
	t.Helper()
	workspace, err := NewWorkspace(t.Context(), nil, limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workspace.Close(); err != nil {
			t.Error(err)
		}
	})
	return workspace
}

func arithmeticParse(t *testing.T, workspace *Workspace, text string) *Int {
	t.Helper()
	value, err := workspace.Parse(t.Context(), text, 10)
	if err != nil {
		t.Fatalf("Parse(%q): %v", text, err)
	}
	return value
}

func arithmeticDecimal(t *testing.T, value *Int, expected string) {
	t.Helper()
	text, err := value.AppendDecimal(t.Context(), make([]byte, 0, value.Digits()+1))
	if err != nil {
		t.Fatal(err)
	}
	if string(text) != expected {
		t.Fatalf("decimal = %s, want %s", text, expected)
	}
	if expected == "0" && value.Sign() != 0 {
		t.Errorf("zero sign = %d", value.Sign())
	}
}

func TestArithmeticSignedCarryBorrow(t *testing.T) {
	cases := []struct {
		name, operation, left, right, expected string
	}{
		{"carry chain", "add", "999999999999999999999999999", "1", "1000000000000000000000000000"},
		{"negative carry", "add", "-999999999999999999", "-1", "-1000000000000000000"},
		{"opposite cancel", "add", "999999999999999999", "-999999999999999999", "0"},
		{"negative larger", "add", "1", "-1000000000000000000", "-999999999999999999"},
		{"positive larger", "add", "1000000000000000000", "-1", "999999999999999999"},
		{"borrow chain", "sub", "1000000000000000000000000000", "1", "999999999999999999999999999"},
		{"negative difference", "sub", "1", "1000000000000000000", "-999999999999999999"},
		{"subtract negative", "sub", "-999999999999999999", "-1000000000000000000", "1"},
		{"subtract self", "sub", "-1000000000000000000", "-1000000000000000000", "0"},
		{"zero minus negative", "sub", "0", "-1", "1"},
		{"zero minus positive", "sub", "0", "1", "-1"},
		{"zero sum", "add", "0", "0", "0"},
		{"carry product", "mul", "999999999999999999", "999999999999999999", "999999999999999998000000000000000001"},
		{"mixed product", "mul", "-123456789", "987654321", "-121932631112635269"},
		{"negative product", "mul", "-1000000000", "-1000000000", "1000000000000000000"},
		{"zero product", "mul", "0", "-999999999999999999", "0"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			workspace := arithmeticWorkspace(t, 1<<20)
			left := arithmeticParse(t, workspace, test.left)
			right := arithmeticParse(t, workspace, test.right)
			operation := workspace.Add
			switch test.operation {
			case "sub":
				operation = workspace.Sub
			case "mul":
				operation = workspace.Mul
			}
			baseline := workspace.UsedBytes()
			result, err := operation(t.Context(), left, right)
			if err != nil {
				t.Fatal(err)
			}
			arithmeticDecimal(t, result, test.expected)
			if err := result.Close(); err != nil {
				t.Fatal(err)
			}
			if workspace.UsedBytes() != baseline {
				t.Fatalf("result close retained %d bytes", workspace.UsedBytes()-baseline)
			}
			arithmeticDecimal(t, left, test.left)
			arithmeticDecimal(t, right, test.right)
		})
	}
}

func TestArithmeticUnaryOwnership(t *testing.T) {
	for _, text := range []string{"0", "-9223372036854775808", "9223372036854775808"} {
		t.Run(text, func(t *testing.T) {
			owner := arithmeticWorkspace(t, 1<<20)
			workspace := arithmeticWorkspace(t, 1<<20)
			value := arithmeticParse(t, owner, text)
			negative, err := workspace.Neg(t.Context(), value)
			if err != nil {
				t.Fatal(err)
			}
			absolute, err := workspace.Abs(t.Context(), value)
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			wantNegative := strings.TrimPrefix(text, "-")
			if text != "0" && !strings.HasPrefix(text, "-") {
				wantNegative = "-" + text
			}
			arithmeticDecimal(t, negative, wantNegative)
			arithmeticDecimal(t, absolute, strings.TrimPrefix(text, "-"))
		})
	}
}

func TestArithmeticPowers(t *testing.T) {
	cases := []struct {
		value    string
		exponent uint64
		expected string
	}{
		{"0", 0, "1"},
		{"0", 100, "0"},
		{"-2", 1, "-2"},
		{"-2", 63, "-9223372036854775808"},
		{"-2", 64, "18446744073709551616"},
		{"999999999", 2, "999999998000000001"},
		{"-1", ^uint64(0), "-1"},
		{"-1", ^uint64(0) - 1, "1"},
		{"123456789", 0, "1"},
	}
	for _, test := range cases {
		t.Run(test.value+"/"+test.expected, func(t *testing.T) {
			workspace := arithmeticWorkspace(t, 1<<20)
			value := arithmeticParse(t, workspace, test.value)
			baseline := workspace.UsedBytes()
			result, err := workspace.Pow(t.Context(), value, test.exponent)
			if err != nil {
				t.Fatal(err)
			}
			arithmeticDecimal(t, result, test.expected)
			if err := result.Close(); err != nil {
				t.Fatal(err)
			}
			if workspace.UsedBytes() != baseline {
				t.Fatalf("power retained intermediates: %d bytes", workspace.UsedBytes()-baseline)
			}
			arithmeticDecimal(t, value, test.value)
		})
	}
}

func TestArithmeticSeededOracle(t *testing.T) {
	workspace := arithmeticWorkspace(t, 1<<20)
	random := rand.New(rand.NewPCG(31, 97))
	for range 100 {
		leftOracle := new(big.Int).SetUint64(random.Uint64())
		leftOracle.Lsh(leftOracle, uint(random.IntN(180)))
		rightOracle := new(big.Int).SetUint64(random.Uint64())
		rightOracle.Lsh(rightOracle, uint(random.IntN(180)))
		if random.IntN(2) == 0 {
			leftOracle.Neg(leftOracle)
		}
		if random.IntN(2) == 0 {
			rightOracle.Neg(rightOracle)
		}
		left := arithmeticParse(t, workspace, leftOracle.String())
		right := arithmeticParse(t, workspace, rightOracle.String())
		for _, operation := range []struct {
			apply    func(context.Context, *Int, *Int) (*Int, error)
			expected *big.Int
		}{
			{workspace.Add, new(big.Int).Add(leftOracle, rightOracle)},
			{workspace.Sub, new(big.Int).Sub(leftOracle, rightOracle)},
			{workspace.Mul, new(big.Int).Mul(leftOracle, rightOracle)},
		} {
			result, err := operation.apply(t.Context(), left, right)
			if err != nil {
				t.Fatal(err)
			}
			arithmeticDecimal(t, result, operation.expected.String())
			if err := result.Close(); err != nil {
				t.Fatal(err)
			}
		}
		arithmeticDecimal(t, left, leftOracle.String())
		arithmeticDecimal(t, right, rightOracle.String())
		if err := errors.Join(left.Close(), right.Close()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArithmeticPowerAdmissionCleanup(t *testing.T) {
	owner := arithmeticWorkspace(t, 1<<20)
	value := arithmeticParse(t, owner, "-2")
	header := uint64(unsafe.Sizeof(Workspace{}))
	ceiling := header + 4*uint64(unsafe.Sizeof(Int{})) + 128
	var failed, succeeded bool
	for limit := header; limit <= ceiling; limit += 4 {
		workspace := arithmeticWorkspace(t, limit)
		baseline := workspace.UsedBytes()
		result, err := workspace.Pow(t.Context(), value, 64)
		if err != nil {
			failed = true
			if !errors.Is(err, ErrLimit) || result != nil {
				t.Fatalf("limit %d: result %v, error %v", limit, result, err)
			}
		} else {
			succeeded = true
			arithmeticDecimal(t, result, "18446744073709551616")
			if err := result.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if workspace.UsedBytes() != baseline {
			t.Fatalf("limit %d: retained %d bytes", limit, workspace.UsedBytes()-baseline)
		}
		arithmeticDecimal(t, value, "-2")
	}
	if !failed || !succeeded {
		t.Fatalf("admission boundary not crossed: failed=%v succeeded=%v", failed, succeeded)
	}
}

type arithmeticCancelContext struct {
	context.Context
	cancel       context.CancelFunc
	remaining    int
	workspace    *Workspace
	usedAtCancel uint64
}

func (ctx *arithmeticCancelContext) Err() error {
	ctx.remaining--
	if ctx.remaining == 0 {
		ctx.usedAtCancel = ctx.workspace.UsedBytes()
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestArithmeticCancellationReleasesIntermediates(t *testing.T) {
	owner := arithmeticWorkspace(t, 1<<20)
	text := strings.Repeat("9", 5400)
	value := arithmeticParse(t, owner, text)
	one := arithmeticParse(t, owner, "1")
	for _, name := range []string{"add", "sub", "mul", "neg", "abs", "pow"} {
		t.Run(name, func(t *testing.T) {
			workspace := arithmeticWorkspace(t, 1<<20)
			baseline := workspace.UsedBytes()
			sawAllocatedCancellation := false
			for threshold := 1; threshold <= 16; threshold++ {
				parent, cancel := context.WithCancel(t.Context())
				ctx := &arithmeticCancelContext{Context: parent, cancel: cancel, remaining: threshold, workspace: workspace}
				var result *Int
				var err error
				switch name {
				case "add":
					result, err = workspace.Add(ctx, value, one)
				case "sub":
					result, err = workspace.Sub(ctx, value, one)
				case "mul":
					result, err = workspace.Mul(ctx, value, value)
				case "neg":
					result, err = workspace.Neg(ctx, value)
				case "abs":
					result, err = workspace.Abs(ctx, value)
				case "pow":
					result, err = workspace.Pow(ctx, value, 5)
				}
				cancel()
				if err != nil {
					if !errors.Is(err, context.Canceled) || result != nil {
						t.Fatalf("threshold %d: result %v, error %v", threshold, result, err)
					}
					sawAllocatedCancellation = sawAllocatedCancellation || ctx.usedAtCancel > baseline
				} else if err := result.Close(); err != nil {
					t.Fatal(err)
				}
				if workspace.UsedBytes() != baseline {
					t.Fatalf("threshold %d: retained %d bytes", threshold, workspace.UsedBytes()-baseline)
				}
				arithmeticDecimal(t, value, text)
				arithmeticDecimal(t, one, "1")
			}
			if !sawAllocatedCancellation {
				t.Fatal("no cancellation exercised owned intermediate cleanup")
			}
		})
	}
}

func TestArithmeticAdmissionPreservesBorrowedOperands(t *testing.T) {
	owner := arithmeticWorkspace(t, 1<<20)
	left := arithmeticParse(t, owner, "999999999999999999")
	right := arithmeticParse(t, owner, "1")
	limit := uint64(unsafe.Sizeof(Workspace{})) + uint64(unsafe.Sizeof(Int{}))
	workspace := arithmeticWorkspace(t, limit)
	baseline := workspace.UsedBytes()
	for _, operation := range []struct {
		name  string
		apply func(context.Context) (*Int, error)
	}{
		{"add", func(ctx context.Context) (*Int, error) { return workspace.Add(ctx, left, right) }},
		{"sub", func(ctx context.Context) (*Int, error) { return workspace.Sub(ctx, left, right) }},
		{"mul", func(ctx context.Context) (*Int, error) { return workspace.Mul(ctx, left, right) }},
		{"neg", func(ctx context.Context) (*Int, error) { return workspace.Neg(ctx, left) }},
		{"abs", func(ctx context.Context) (*Int, error) { return workspace.Abs(ctx, left) }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			result, err := operation.apply(t.Context())
			if result != nil || !errors.Is(err, ErrLimit) {
				t.Fatalf("result %v, error %v; want nil, ErrLimit", result, err)
			}
			if workspace.UsedBytes() != baseline {
				t.Fatalf("failed operation retained %d bytes", workspace.UsedBytes()-baseline)
			}
			arithmeticDecimal(t, left, "999999999999999999")
			arithmeticDecimal(t, right, "1")
		})
	}
}
