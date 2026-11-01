package decimalint

import (
	"context"
	"errors"
	"math/big"
	"math/rand/v2"
	"strings"
	"testing"
)

func divisionWorkspace(t *testing.T) *Workspace {
	t.Helper()
	workspace, err := NewWorkspace(t.Context(), nil, 8<<20)
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

func divisionParse(t *testing.T, workspace *Workspace, text string) *Int {
	t.Helper()
	value, err := workspace.Parse(t.Context(), text, 10)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func divisionBig(t *testing.T, value *Int) *big.Int {
	t.Helper()
	text, err := value.AppendDecimal(t.Context(), make([]byte, 0, value.Digits()+1))
	if err != nil {
		t.Fatal(err)
	}
	result, ok := new(big.Int).SetString(string(text), 10)
	if !ok {
		t.Fatalf("invalid decimal output %q", text)
	}
	return result
}

func checkDivision(t *testing.T, workspace *Workspace, leftText, rightText string) {
	t.Helper()
	baseline := workspace.UsedBytes()
	left, right := divisionParse(t, workspace, leftText), divisionParse(t, workspace, rightText)
	a, _ := new(big.Int).SetString(leftText, 10)
	b, _ := new(big.Int).SetString(rightText, 10)
	quotient, remainder, err := workspace.QuoRem(t.Context(), left, right)
	if err != nil {
		t.Fatal(err)
	}
	q, r := divisionBig(t, quotient), divisionBig(t, remainder)
	wantQ, wantR := new(big.Int), new(big.Int)
	wantQ.QuoRem(a, b, wantR)
	if q.Cmp(wantQ) != 0 || r.Cmp(wantR) != 0 {
		t.Fatalf("%s / %s = (%s, %s), want (%s, %s)", a, b, q, r, wantQ, wantR)
	}
	identity := new(big.Int).Add(new(big.Int).Mul(q, b), r)
	wrongSign := r.Sign() != 0 && r.Sign() != a.Sign()
	if identity.Cmp(a) != 0 || new(big.Int).Abs(r).Cmp(new(big.Int).Abs(b)) >= 0 || wrongSign {
		t.Fatalf("division invariant failed for %s / %s", a, b)
	}
	if divisionBig(t, left).Cmp(a) != 0 || divisionBig(t, right).Cmp(b) != 0 {
		t.Fatal("division mutated input")
	}
	if err := errors.Join(left.Close(), right.Close(), quotient.Close()); err != nil {
		t.Fatal(err)
	}
	if divisionBig(t, remainder).Cmp(wantR) != 0 {
		t.Fatal("remainder aliases released storage")
	}
	if err := remainder.Close(); err != nil {
		t.Fatal(err)
	}
	if workspace.UsedBytes() != baseline {
		t.Fatal("division retained scratch storage")
	}
}

func TestQuoRemSignedBoundaries(t *testing.T) {
	workspace := divisionWorkspace(t)
	for _, pair := range [][2]string{
		{"0", "1"}, {"1", "999999999999999999"}, {"999999999999999999", "1"},
		{"-7", "3"}, {"7", "-3"}, {"-7", "-3"}, {"-9", "3"},
		{"1000000000000000000000000000", "1000000000000000001"},
		{"999999999999999999999999999999999999", "999999999999999999"},
		{"500000000000000000000000000", "500000000000000001"},
		// A matching top pair hides the low divisor limb: the estimate
		// passes the two-limb test but needs the final subtraction add-back.
		{"500000000000000000000000000", "500000000000000000000000001"},
		{"500000000000000000000000000000000000", "500000000000000000000000001"},
		{"999999999000000000000000000", "999999999999999999"},
	} {
		checkDivision(t, workspace, pair[0], pair[1])
	}
}

func TestDivisionAdvancedSeededOracle(t *testing.T) {
	workspace := divisionWorkspace(t)
	random := rand.New(rand.NewPCG(73, 109))
	for iteration := range 160 {
		decimal := func(digits int) string {
			text := make([]byte, digits)
			for i := range text {
				text[i] = byte('0' + random.IntN(10))
			}
			text[0] = byte('1' + random.IntN(9))
			return string(text)
		}
		aText, bText := decimal(1+random.IntN(240)), decimal(1+random.IntN(120))
		if iteration%2 == 0 {
			aText = "-" + aText
		}
		if iteration%3 == 0 {
			bText = "-" + bText
		}
		checkDivision(t, workspace, aText, bText)
		baseline := workspace.UsedBytes()
		a, b := divisionParse(t, workspace, aText), divisionParse(t, workspace, bText)
		bigA, bigB := divisionBig(t, a), divisionBig(t, b)
		gcd, err := workspace.GCD(t.Context(), a, b)
		if err != nil {
			t.Fatal(err)
		}
		want := new(big.Int).GCD(nil, nil, bigA, bigB)
		if got := divisionBig(t, gcd); got.Cmp(want) != 0 {
			t.Fatalf("gcd(%s,%s) = %s, want %s", bigA, bigB, got, want)
		}
		positive := divisionParse(t, workspace, new(big.Int).Abs(bigA).String())
		root, err := workspace.Sqrt(t.Context(), positive)
		if err != nil {
			t.Fatal(err)
		}
		gotRoot := divisionBig(t, root)
		want.Sqrt(new(big.Int).Abs(bigA))
		if gotRoot.Cmp(want) != 0 {
			t.Fatalf("sqrt(%s) = %s, want %s", bigA, gotRoot, want)
		}
		lower := new(big.Int).Mul(gotRoot, gotRoot)
		upper := new(big.Int).Add(gotRoot, big.NewInt(1))
		upper.Mul(upper, upper)
		if lower.Cmp(new(big.Int).Abs(bigA)) > 0 || upper.Cmp(new(big.Int).Abs(bigA)) <= 0 {
			t.Fatal("square root floor invariant failed")
		}
		if err := errors.Join(a.Close(), b.Close(), gcd.Close(), positive.Close(), root.Close()); err != nil {
			t.Fatal(err)
		}
		if workspace.UsedBytes() != baseline {
			t.Fatal("advanced arithmetic retained scratch")
		}
	}
}

func TestAdvancedZeroAndSquareBoundaries(t *testing.T) {
	workspace := divisionWorkspace(t)
	zero := divisionParse(t, workspace, "0")
	negative := divisionParse(t, workspace, "-144")
	for _, pair := range [][2]*Int{{zero, zero}, {negative, zero}, {zero, negative}} {
		gcd, err := workspace.GCD(t.Context(), pair[0], pair[1])
		if err != nil {
			t.Fatal(err)
		}
		want := int64(144)
		if pair[0] == zero && pair[1] == zero {
			want = 0
		}
		if divisionBig(t, gcd).Cmp(big.NewInt(want)) != 0 {
			t.Fatal("incorrect zero gcd")
		}
		if err := gcd.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, square := range []string{"0", "1", "4", "1000000000000000000", "152415787532388367501905199875019052100"} {
		center, _ := new(big.Int).SetString(square, 10)
		for delta := int64(-1); delta <= 1; delta++ {
			input := new(big.Int).Add(center, big.NewInt(delta))
			if input.Sign() < 0 {
				continue
			}
			value := divisionParse(t, workspace, input.String())
			root, err := workspace.Sqrt(t.Context(), value)
			if err != nil {
				t.Fatal(err)
			}
			if divisionBig(t, root).Cmp(new(big.Int).Sqrt(input)) != 0 {
				t.Fatalf("incorrect sqrt(%s)", input)
			}
			if err := errors.Join(value.Close(), root.Close()); err != nil {
				t.Fatal(err)
			}
		}
	}
	baseline := workspace.UsedBytes()
	if q, r, err := workspace.QuoRem(t.Context(), negative, zero); !errors.Is(err, ErrDivisionByZero) || q != nil || r != nil {
		t.Fatalf("zero divisor: %v", err)
	}
	if root, err := workspace.Sqrt(t.Context(), negative); !errors.Is(err, ErrNegativeSqrt) || root != nil {
		t.Fatalf("negative root: %v", err)
	}
	if workspace.UsedBytes() != baseline {
		t.Fatal("domain errors retained storage")
	}
}

type divisionCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (ctx *divisionCancelContext) Err() error {
	if ctx.remaining == 0 {
		ctx.cancel()
	} else {
		ctx.remaining--
	}
	return ctx.Context.Err()
}

func TestDivisionAdvancedCancellationCleanup(t *testing.T) {
	inputs := divisionWorkspace(t)
	left := divisionParse(t, inputs, strings.Repeat("987654321", 700))
	right := divisionParse(t, inputs, strings.Repeat("123456789", 300))
	for _, operation := range []struct {
		name string
		run  func(context.Context, *Workspace) (*Int, error)
	}{
		{"quo", func(ctx context.Context, w *Workspace) (*Int, error) { return w.Quo(ctx, left, right) }},
		{"rem", func(ctx context.Context, w *Workspace) (*Int, error) { return w.Rem(ctx, left, right) }},
		{"gcd", func(ctx context.Context, w *Workspace) (*Int, error) { return w.GCD(ctx, left, right) }},
		{"sqrt", func(ctx context.Context, w *Workspace) (*Int, error) { return w.Sqrt(ctx, left) }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			workspace := divisionWorkspace(t)
			baseline := workspace.UsedBytes()
			for _, checks := range []int{0, 12, 40} {
				base, cancel := context.WithCancel(t.Context())
				ctx := &divisionCancelContext{Context: base, cancel: cancel, remaining: checks}
				result, err := operation.run(ctx, workspace)
				cancel()
				if !errors.Is(err, context.Canceled) || result != nil {
					t.Fatalf("cancellation after %d checks: result=%v err=%v", checks, result, err)
				}
				if workspace.UsedBytes() != baseline {
					t.Fatal("cancellation retained storage")
				}
			}
		})
	}
}

func TestDivisionAdvancedBudgetCleanup(t *testing.T) {
	inputs := divisionWorkspace(t)
	left := divisionParse(t, inputs, "987654321012345678909876543210123456789")
	right := divisionParse(t, inputs, "1234567890123456789")
	for _, operation := range []struct {
		name string
		run  func(*Workspace) (*Int, error)
	}{
		{"quo", func(w *Workspace) (*Int, error) { return w.Quo(t.Context(), left, right) }},
		{"rem", func(w *Workspace) (*Int, error) { return w.Rem(t.Context(), left, right) }},
		{"gcd", func(w *Workspace) (*Int, error) { return w.GCD(t.Context(), left, right) }},
		{"sqrt", func(w *Workspace) (*Int, error) { return w.Sqrt(t.Context(), left) }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			probe := divisionWorkspace(t)
			header := probe.UsedBytes()
			failures, successes := 0, 0
			for allowance := uint64(0); allowance <= 2048; allowance += 4 {
				workspace, err := NewWorkspace(t.Context(), nil, header+allowance)
				if err != nil {
					t.Fatal(err)
				}
				result, err := operation.run(workspace)
				if err != nil {
					if !errors.Is(err, ErrLimit) || result != nil {
						t.Fatalf("budget failure: result=%v err=%v", result, err)
					}
					failures++
				} else {
					successes++
					if err := result.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if workspace.UsedBytes() != header {
					t.Fatalf("budget %d retained %d bytes", allowance, workspace.UsedBytes()-header)
				}
				if err := workspace.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if failures == 0 || successes == 0 {
				t.Fatalf("budget sweep: %d failures, %d successes", failures, successes)
			}
		})
	}
}

func TestDivisionOutputsOutliveBorrowedWorkspace(t *testing.T) {
	inputs := divisionWorkspace(t)
	outputs := divisionWorkspace(t)
	left := divisionParse(t, inputs, "-1000000000000000001")
	right := divisionParse(t, inputs, "1000000000")
	quotient, err := outputs.Quo(t.Context(), left, right)
	if err != nil {
		t.Fatal(err)
	}
	remainder, err := outputs.Rem(t.Context(), left, right)
	if err != nil {
		t.Fatal(err)
	}
	if err := inputs.Close(); err != nil {
		t.Fatal(err)
	}
	if divisionBig(t, quotient).Cmp(big.NewInt(-1000000000)) != 0 {
		t.Fatal("quotient changed after borrowed workspace closed")
	}
	if err := quotient.Close(); err != nil {
		t.Fatal(err)
	}
	if divisionBig(t, remainder).Cmp(big.NewInt(-1)) != 0 {
		t.Fatal("remainder changed after quotient closed")
	}
	if err := remainder.Close(); err != nil {
		t.Fatal(err)
	}
}
