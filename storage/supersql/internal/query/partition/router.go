package partition

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

var (
	ErrInvalidHierarchy = errors.New("invalid partition hierarchy")
	ErrOverlappingBound = errors.New("partition bounds overlap")
	ErrNoPartition      = &model.Error{Code: "23514", Message: "no partition found for row"}
	ErrLimit            = errors.New("partition routing resource limit exceeded")
)

const hashPartitionSeed = uint64(0x7a5b22367996dcfd)

type Limits struct {
	MaxChildren int
	MaxKeys     int
	MaxBytes    uint64
}

func DefaultLimits() Limits {
	return Limits{MaxChildren: 4096, MaxKeys: 32, MaxBytes: 64 << 20}
}

type Target struct {
	Ref storage.ObjectRef
	OID model.OID
}

// Options borrows Encoder and owns no catalog snapshot. Children and their
// descriptors must all come from the same immutable catalog snapshot as Parent.
type Options struct {
	Parent   *model.Descriptor
	Children []*model.Descriptor
	Encoder  storage.KeyEncoder
	Builtin  builtin.ResolveOptions
	Limits   Limits
}

type Router struct {
	strategy byte
	layout   []storage.KeyColumn
	encoder  storage.KeyEncoder
	children []child
	fallback int
	hashes   []builtin.Callable
	limits   Limits
	ownedBytes uint64
}

type child struct {
	target Target
	bound  model.PartitionBound
	list   [][]byte
	lower  encodedCut
	upper  encodedCut
}

type encodedCut struct {
	prefix   []byte
	infinity int8
	after    bool
}

func NewRouter(ctx context.Context, options Options) (*Router, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limits := options.Limits
	if options.Parent == nil || options.Encoder == nil || limits.MaxChildren <= 0 || limits.MaxKeys <= 0 || limits.MaxBytes == 0 {
		return nil, ErrInvalidHierarchy
	}
	parent, ok := options.Parent.Table()
	if !ok || options.Parent.State() != model.StatePublic || parent.PartitionStrategy == 0 || len(parent.PartitionKeys) == 0 {
		return nil, ErrInvalidHierarchy
	}
	if len(parent.PartitionKeys) > limits.MaxKeys || len(options.Children) > limits.MaxChildren {
		return nil, ErrLimit
	}
	router := &Router{
		strategy: parent.PartitionStrategy, encoder: options.Encoder, fallback: -1, limits: limits,
		layout: make([]storage.KeyColumn, len(parent.PartitionKeys)),
		children: make([]child, 0, len(options.Children)),
	}
	if router.strategy == 'h' {
		router.hashes = make([]builtin.Callable, 0, len(parent.PartitionKeys))
	}
	used := uint64(unsafe.Sizeof(*router)) +
		uint64(cap(router.layout))*uint64(unsafe.Sizeof(storage.KeyColumn{})) +
		uint64(cap(router.children))*uint64(unsafe.Sizeof(child{})) +
		uint64(cap(router.hashes))*uint64(unsafe.Sizeof(builtin.Callable{}))
	if used > limits.MaxBytes {
		return nil, ErrLimit
	}
	for i, key := range parent.PartitionKeys {
		column := key.Column
		if column == 0 {
			column = model.ColumnID(i + 1)
		}
		layout, size, err := model.CopyPhysicalTypeLayout(ctx, model.TypeLayout{Type: key.Type, Children: key.Children}, limits.MaxBytes-used)
		if err != nil {
			return nil, err
		}
		used += size
		router.layout[i] = storage.KeyColumn{ColumnID: column, Type: layout.Type, Children: layout.Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
		if key.Type.ID == 0 || !key.Type.Storage.Valid() || key.OperatorClass == 0 {
			return nil, ErrInvalidHierarchy
		}
		class, found := builtin.LookupOperatorClass(key.OperatorClass)
		if !found || class.InputType != key.Type.ID {
			return nil, ErrInvalidHierarchy
		}
		if router.strategy == 'h' {
			if class.Method != "hash" || class.HashExtendedFunction == 0 {
				return nil, ErrInvalidHierarchy
			}
			integer := model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}
			call, err := builtin.ResolveTyped(ctx, class.HashExtendedFunction, []model.TypeLayout{{Type: key.Type, Children: key.Children}, integer}, integer, options.Builtin, limits.MaxBytes-used)
			if err != nil {
				return nil, err
			}
			router.hashes = append(router.hashes, call)
			used += call.OwnedBytes()
			if used > limits.MaxBytes {
				return nil, ErrLimit
			}
			continue
		}
		if class.Method != "btree" {
			return nil, ErrInvalidHierarchy
		}
		certified, ok := options.Encoder.(storage.IndexKeyEncoder)
		if !ok {
			return nil, ErrInvalidHierarchy
		}
		if err := certified.ValidateOperatorClass(ctx, model.TypeLayout{Type: key.Type, Children: key.Children}, key.OperatorClass); err != nil {
			return nil, err
		}
	}
	for _, descriptor := range options.Children {
		compiled, bytesUsed, err := router.compileChild(ctx, options.Parent, descriptor, limits.MaxBytes-used)
		if err != nil {
			return nil, err
		}
		used += bytesUsed
		if used > limits.MaxBytes {
			return nil, ErrLimit
		}
		if compiled.bound.Default {
			if router.fallback >= 0 {
				return nil, ErrOverlappingBound
			}
			router.fallback = len(router.children)
		}
		router.children = append(router.children, compiled)
	}
	if err := router.validateDisjoint(ctx); err != nil {
		return nil, err
	}
	router.ownedBytes = used
	return router, nil
}
// OwnedBytes reports immutable routing metadata retained by the router.
func (router *Router) OwnedBytes() uint64 {
	if router == nil {
		return 0
	}
	return router.ownedBytes
}


func (router *Router) compileChild(ctx context.Context, parent, descriptor *model.Descriptor, maximum uint64) (child, uint64, error) {
	if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.Kind() != model.ObjectTable {
		return child{}, 0, ErrInvalidHierarchy
	}
	metadata, ok := descriptor.Table()
	if !ok || metadata.PartitionBound == nil || metadata.PartitionState != model.PartitionAttached || !slices.Contains(metadata.Parents, parent.ID()) {
		return child{}, 0, ErrInvalidHierarchy
	}
	bound := metadata.PartitionBound
	if bound.Strategy != router.strategy {
		return child{}, 0, ErrInvalidHierarchy
	}
	physical := descriptor.Physical()
	target := Target{Ref: storage.ObjectRef{ObjectID: descriptor.ID(), DescriptorVersion: descriptor.Version(), Generation: physical.Generation, EncodingVersion: physical.EncodingVersion}, OID: descriptor.OID()}
	if err := target.Ref.Validate(); err != nil {
		return child{}, 0, err
	}
	compiled := child{target: target, bound: model.PartitionBound{
		Strategy: bound.Strategy, Default: bound.Default, Modulus: bound.Modulus, Remainder: bound.Remainder,
	}}
	var used uint64
	if bound.Default {
		return compiled, used, nil
	}
	switch router.strategy {
	case 'l':
		if len(router.layout) != 1 || len(bound.ListValues) == 0 {
			return child{}, 0, ErrInvalidHierarchy
		}
		elementBytes := uint64(unsafe.Sizeof([]byte{}))
		if uint64(len(bound.ListValues)) > maximum/elementBytes {
			return child{}, 0, ErrLimit
		}
		listBytes := uint64(len(bound.ListValues)) * elementBytes
		if listBytes > maximum {
			return child{}, 0, ErrLimit
		}
		compiled.list = make([][]byte, len(bound.ListValues))
		used += listBytes
		for i, datum := range bound.ListValues {
			if !datumMatches(datum, router.layout[0], false) {
				return child{}, 0, ErrInvalidHierarchy
			}
			encoded, err := router.encoder.EncodePrefix(ctx, router.layout, []model.Value{datum.Value}, maximum-used)
			if err != nil {
				return child{}, 0, err
			}
			used += uint64(len(encoded))
			if used > maximum {
				return child{}, 0, ErrLimit
			}
			compiled.list[i] = encoded
		}
	case 'r':
		if len(bound.Lower) != len(router.layout) || len(bound.Upper) != len(router.layout) {
			return child{}, 0, ErrInvalidHierarchy
		}
		var err error
		compiled.lower, err = router.compileEndpoint(ctx, bound.Lower, maximum-used)
		if err != nil {
			return child{}, 0, err
		}
		used += uint64(len(compiled.lower.prefix))
		compiled.upper, err = router.compileEndpoint(ctx, bound.Upper, maximum-used)
		if err != nil {
			return child{}, 0, err
		}
		used += uint64(len(compiled.upper.prefix))
		if compareCuts(compiled.lower, compiled.upper) >= 0 {
			return child{}, 0, ErrInvalidHierarchy
		}
	case 'h':
		if bound.Modulus == 0 || bound.Remainder >= bound.Modulus {
			return child{}, 0, ErrInvalidHierarchy
		}
	default:
		return child{}, 0, ErrInvalidHierarchy
	}
	return compiled, used, nil
}

func datumMatches(datum model.PartitionDatum, key storage.KeyColumn, sentinel bool) bool {
	if !model.TypeLayoutsEqual(datum.Layout, model.TypeLayout{Type: key.Type, Children: key.Children}) {
		return false
	}
	if datum.Kind == model.PartitionDatumValue {
		return datum.Value.Type == key.Type.Storage
	}
	return sentinel && (datum.Kind == model.PartitionDatumMin || datum.Kind == model.PartitionDatumMax)
}

func (router *Router) compileEndpoint(ctx context.Context, values []model.PartitionDatum, maximum uint64) (encodedCut, error) {
	finite := make([]model.Value, 0, len(values))
	sentinel := model.PartitionDatumKind(0)
	for i, datum := range values {
		if !datumMatches(datum, router.layout[i], true) {
			return encodedCut{}, ErrInvalidHierarchy
		}
		if datum.Kind == model.PartitionDatumValue {
			if sentinel != 0 || datum.Value.IsNull {
				return encodedCut{}, ErrInvalidHierarchy
			}
			finite = append(finite, datum.Value)
			continue
		}
		if sentinel != 0 && sentinel != datum.Kind {
			return encodedCut{}, ErrInvalidHierarchy
		}
		sentinel = datum.Kind
	}
	if len(finite) == 0 {
		if sentinel == model.PartitionDatumMin {
			return encodedCut{infinity: -1}, nil
		}
		if sentinel == model.PartitionDatumMax {
			return encodedCut{infinity: 1}, nil
		}
		return encodedCut{}, ErrInvalidHierarchy
	}
	prefix, err := router.encoder.EncodePrefix(ctx, router.layout, finite, maximum)
	if err != nil {
		return encodedCut{}, err
	}
	cut := encodedCut{prefix: prefix}
	if sentinel == model.PartitionDatumMax {
		cut.after = true
	}
	return cut, nil
}

func (router *Router) validateDisjoint(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch router.strategy {
	case 'l':
		seen := make(map[string]struct{})
		for _, child := range router.children {
			for _, value := range child.list {
				key := string(value)
				if _, duplicate := seen[key]; duplicate {
					return ErrOverlappingBound
				}
				seen[key] = struct{}{}
			}
		}
	case 'r':
		ordered := make([]child, 0, len(router.children))
		for _, child := range router.children {
			if !child.bound.Default {
				ordered = append(ordered, child)
			}
		}
		slices.SortFunc(ordered, func(left, right child) int { return compareCuts(left.lower, right.lower) })
		for i := 1; i < len(ordered); i++ {
			if compareCuts(ordered[i-1].upper, ordered[i].lower) > 0 {
				return ErrOverlappingBound
			}
		}
	case 'h':
		for i, left := range router.children {
			if left.bound.Default {
				continue
			}
			for _, right := range router.children[:i] {
				if right.bound.Default {
					continue
				}
				divisor := gcd(left.bound.Modulus, right.bound.Modulus)
				if left.bound.Remainder%divisor == right.bound.Remainder%divisor {
					return ErrOverlappingBound
				}
			}
		}
	}
	return ctx.Err()
}

func gcd(left, right uint32) uint32 {
	for right != 0 {
		left, right = right, left%right
	}
	return left
}

func compareCuts(left, right encodedCut) int {
	if left.infinity != 0 || right.infinity != 0 {
		return cmp.Compare(left.infinity, right.infinity)
	}
	if bytes.Equal(left.prefix, right.prefix) {
		if left.after == right.after {
			return 0
		}
		if left.after {
			return 1
		}
		return -1
	}
	if bytes.HasPrefix(right.prefix, left.prefix) {
		if left.after {
			return 1
		}
		return -1
	}
	if bytes.HasPrefix(left.prefix, right.prefix) {
		if right.after {
			return -1
		}
		return 1
	}
	return bytes.Compare(left.prefix, right.prefix)
}

// Route accepts evaluated partition-key values in parent-key order. It returns
// the real child relation identity; callers must never substitute the root.
func (router *Router) Route(ctx context.Context, values []model.Value) (Target, error) {
	if router == nil || len(values) != len(router.layout) {
		return Target{}, ErrInvalidHierarchy
	}
	if err := ctx.Err(); err != nil {
		return Target{}, err
	}
	for i, value := range values {
		if value.Type != router.layout[i].Type.Storage {
			return Target{}, ErrInvalidHierarchy
		}
	}
	var match int = -1
	switch router.strategy {
	case 'l':
		encoded, err := router.encoder.EncodePrefix(ctx, router.layout, values, router.limits.MaxBytes)
		if err != nil {
			return Target{}, err
		}
		for i, child := range router.children {
			for _, candidate := range child.list {
				if bytes.Equal(candidate, encoded) {
					match = i
					break
				}
			}
			if match >= 0 {
				break
			}
		}
	case 'r':
		if !slices.ContainsFunc(values, func(value model.Value) bool { return value.IsNull }) {
			encoded, err := router.encoder.EncodePrefix(ctx, router.layout, values, router.limits.MaxBytes)
			if err != nil {
				return Target{}, err
			}
			point := encodedCut{prefix: encoded}
			for i, child := range router.children {
				if child.bound.Default {
					continue
				}
				if compareCuts(child.lower, point) <= 0 && compareCuts(point, child.upper) < 0 {
					match = i
					break
				}
			}
		}
	case 'h':
		remainder, err := router.hash(ctx, values)
		if err != nil {
			return Target{}, err
		}
		for i, child := range router.children {
			if !child.bound.Default && remainder%uint64(child.bound.Modulus) == uint64(child.bound.Remainder) {
				match = i
				break
			}
		}
	default:
		return Target{}, ErrInvalidHierarchy
	}
	if match < 0 {
		match = router.fallback
	}
	if match < 0 {
		return Target{}, ErrNoPartition
	}
	return router.children[match].target, nil
}

func (router *Router) hash(ctx context.Context, values []model.Value) (uint64, error) {
	rowHash := uint64(0)
	budget := builtin.CallBudget{OutputBytes: 8, MemoryBytes: min(router.limits.MaxBytes, uint64(math.MaxInt64))}
	for i, value := range values {
		if value.IsNull {
			continue
		}
		result, err := router.hashes[i].Call(ctx, []model.Value{value, model.Int64Val(int64(hashPartitionSeed))}, budget)
		if err != nil {
			return 0, err
		}
		if result.IsNull || result.Type != model.TypeInt64 {
			return 0, fmt.Errorf("%w: hash support returned invalid result", ErrInvalidHierarchy)
		}
		rowHash = hashCombine64(rowHash, uint64(result.IntVal))
	}
	return rowHash, ctx.Err()
}

func hashCombine64(left, right uint64) uint64 {
	return left ^ (right + 0x49a0f4dd15e5a8e3 + (left << 54) + (left >> 7))
}

func (router *Router) Targets() []Target {
	if router == nil {
		return nil
	}
	result := make([]Target, len(router.children))
	for i, child := range router.children {
		result[i] = child.target
	}
	return result
}

// Prune returns only children that can contain the supplied partition-key spans.
// A nil span set means unconstrained. Non-point LIST/HASH spans remain
// conservative; RANGE spans are intersected against immutable child bounds.
func (router *Router) Prune(ctx context.Context, spans []storage.KeySpan) ([]Target, error) {
	if router == nil {
		return nil, ErrInvalidHierarchy
	}
	if len(spans) == 0 {
		return router.Targets(), nil
	}
	if len(spans) > router.limits.MaxChildren {
		return nil, ErrLimit
	}
	selected := make([]bool, len(router.children))
	var used uint64
	for _, span := range spans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		point, exact, bytesUsed, err := router.exactSpanPoint(ctx, span, router.limits.MaxBytes-used)
		if err != nil {
			return nil, err
		}
		used += bytesUsed
		if exact {
			target, err := router.Route(ctx, point)
			if errors.Is(err, ErrNoPartition) {
				continue
			}
			if err != nil {
				return nil, err
			}
			index := slices.IndexFunc(router.children, func(child child) bool { return child.target == target })
			if index < 0 {
				return nil, ErrInvalidHierarchy
			}
			selected[index] = true
			continue
		}
		if router.strategy != 'r' {
			for i := range selected {
				selected[i] = true
			}
			continue
		}
		lower, lowerBytes, err := router.spanCut(ctx, span.Lower, true, router.limits.MaxBytes-used)
		if err != nil {
			return nil, err
		}
		used += lowerBytes
		upper, upperBytes, err := router.spanCut(ctx, span.Upper, false, router.limits.MaxBytes-used)
		if err != nil {
			return nil, err
		}
		used += upperBytes
		if used > router.limits.MaxBytes || compareCuts(lower, upper) >= 0 {
			return nil, ErrInvalidHierarchy
		}
		for i, child := range router.children {
			if child.bound.Default || compareCuts(child.lower, upper) < 0 && compareCuts(lower, child.upper) < 0 {
				selected[i] = true
			}
		}
	}
	result := make([]Target, 0, len(router.children))
	for i, child := range router.children {
		if selected[i] {
			result = append(result, child.target)
		}
	}
	return result, nil
}

func (router *Router) exactSpanPoint(ctx context.Context, span storage.KeySpan, maximum uint64) ([]model.Value, bool, uint64, error) {
	if len(span.Lower.Values) != len(router.layout) || len(span.Upper.Values) != len(router.layout) || !span.Lower.Inclusive || !span.Upper.Inclusive {
		return nil, false, 0, nil
	}
	for i := range span.Lower.Values {
		if span.Lower.Values[i].Type != router.layout[i].Type.Storage || span.Upper.Values[i].Type != router.layout[i].Type.Storage {
			return nil, false, 0, ErrInvalidHierarchy
		}
	}
	left, err := router.encoder.EncodePrefix(ctx, router.layout, span.Lower.Values, maximum)
	if err != nil {
		return nil, false, 0, err
	}
	used := uint64(len(left))
	if used > maximum {
		return nil, false, 0, ErrLimit
	}
	right, err := router.encoder.EncodePrefix(ctx, router.layout, span.Upper.Values, maximum-used)
	if err != nil {
		return nil, false, 0, err
	}
	used += uint64(len(right))
	if used > maximum {
		return nil, false, 0, ErrLimit
	}
	if !bytes.Equal(left, right) {
		return nil, false, used, nil
	}
	return span.Lower.Values, true, used, nil
}

func (router *Router) spanCut(ctx context.Context, bound storage.KeyBound, lower bool, maximum uint64) (encodedCut, uint64, error) {
	if bound.Unbounded {
		if len(bound.Values) != 0 || bound.Inclusive {
			return encodedCut{}, 0, ErrInvalidHierarchy
		}
		if lower {
			return encodedCut{infinity: -1}, 0, nil
		}
		return encodedCut{infinity: 1}, 0, nil
	}
	if len(bound.Values) == 0 {
		return encodedCut{}, 0, ErrInvalidHierarchy
	}
	if len(bound.Values) > len(router.layout) {
		return encodedCut{}, 0, ErrInvalidHierarchy
	}
	for i, value := range bound.Values {
		if value.Type != router.layout[i].Type.Storage || value.IsNull {
			return encodedCut{}, 0, ErrInvalidHierarchy
		}
	}
	encoded, err := router.encoder.EncodePrefix(ctx, router.layout, bound.Values, maximum)
	if err != nil {
		return encodedCut{}, 0, err
	}
	if uint64(len(encoded)) > maximum {
		return encodedCut{}, 0, ErrLimit
	}
	after := !lower && bound.Inclusive || lower && !bound.Inclusive
	return encodedCut{prefix: encoded, after: after}, uint64(len(encoded)), nil
}
