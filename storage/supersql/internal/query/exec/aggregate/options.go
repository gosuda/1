// Package aggregate executes bounded aggregation over projected typed columns.
package aggregate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type OrderKey struct {
	Column     int
	Descending bool
	NullsFirst bool
}

type Phase uint8

const (
	Complete Phase = iota
	Partial
	Final
)

type DirectEvaluator interface {
	Evaluate(context.Context, []model.Value, []int, []model.Value) error
	Close() error
}

type Call struct {
	Function        model.FunctionID
	Combine         model.FunctionID
	InputCollation  model.CollationID
	Arguments       []int
	DirectLayouts   []model.TypeLayout
	DirectEvaluator DirectEvaluator
	FilterColumn    *int
	Distinct        bool
	OrderBy         []OrderKey
}

// OutputLayouts contains grouping columns followed by aggregate results.
// OrderedGroups promises contiguous not-distinct grouping keys. Without that
// promise, hash partitions are consumed completely before their results appear.
// All byte limits are positive. MaxPartitions bounds deferred passes (and sort
// runs), not simultaneously open partitions. At most two aggregation partitions
// are live; DISTINCT and ORDER BY additionally need sort files and readers.
// DirectEvaluator is borrowed and serialized. Evaluate results are borrowed
// until its next call; aggregate finalization consumes them synchronously.
// If OwnDirectEvaluators is set, New takes ownership on every outcome.
// Input ownership transfers on every outcome; Query and Encoder are borrowed.
type Options struct {
	Phase               Phase
	InputLayouts        []model.TypeLayout
	OutputLayouts       []model.TypeLayout
	GroupColumns        []int
	DirectGroupCount    int
	OwnDirectEvaluators bool
	// DirectGroupColumns maps the evaluator's full grouping row to GroupColumns
	// ordinals. A negative ordinal supplies SQL NULL for an omitted grouping key.
	DirectGroupColumns   []int
	Calls                []Call
	Encoder              storage.KeyEncoder
	Query                *runtime.Query
	Builtin              builtin.ResolveOptions
	BatchRows            int
	BatchBytes           uint64
	MaxRecordBytes       uint64
	GroupBytes           uint64
	PartitionMemoryBytes uint64
	WorkBytes            uint64
	MaxPartitions        int
	OrderedGroups        bool
	// Aggregates supplies SQL-defined aggregate declarations. It is consulted
	// only for identities absent from the builtin inventory; nil means this
	// build executes pinned aggregates only.
	Aggregates AggregateResolver
}

type operation uint8

const (
	count operation = iota + 1
	sum
	average
	minimum
	maximum
	bitwiseAnd
	bitwiseOr
	bitwiseXor
	variancePopulation
	varianceSample
	stddevPopulation
	stddevSample
	regrCount
	regrSxx
	regrSyy
	regrSxy
	regrAvgx
	regrAvgy
	regrR2
	regrSlope
	regrIntercept
	covarPopulation
	covarSample
	correlation
	stringAggregate
	anyValue
	arrayAggregate
	arrayAggregateArrays
	jsonAggregate
	rangeUnionAggregate
	rangeIntersectAggregate
	percentileDiscrete
	percentileContinuous
	modeAggregate
	hypotheticalRank
	hypotheticalPercentRank
	hypotheticalCumeDist
	hypotheticalDenseRank
)

type compiledCall struct {
	op operation
	// arguments and inputs are allocated once while compiling the call, sized to
	// its exact arity. A compiled call therefore owns precisely the argument
	// identity it uses, instead of carrying a fixed maximum-width array into
	// every call in the plan.
	arguments       []int
	inputs          []model.Type
	arity           int
	directCount     int
	arrayPercentile bool
	combining       bool
	directEvaluator DirectEvaluator
	filter          int
	distinct        bool
	element         model.DataType
	output          model.Type
	key             []storage.KeyColumn
	order           []OrderKey
	orderKey        []storage.KeyColumn
	json            *jsonAggregatePlan
	rangePlan       *rangeAggregatePlan
	scalar          *scalarAggregatePlan
	user            *userAggregatePlan
}

type UnsupportedError struct {
	Function model.FunctionID
	Reason   string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("aggregate %d is not executable: %s", e.Function, e.Reason)
}

// newCompiledCall allocates the argument identity of one compiled call. The two
// slices are sized to the call's arity and owned by the returned value.
func newCompiledCall(arity int, output model.Type) compiledCall {
	return compiledCall{filter: -1, output: output, arity: arity, arguments: make([]int, arity), inputs: make([]model.Type, arity)}
}

func resource(message string, cause error) error {
	return &model.Error{Code: "53200", Message: message, Cause: errors.Join(runtime.ErrBudget, cause)}
}

func (c *cursor) configure(ctx context.Context) error {
	o := &c.options
	missingInput := c.input == nil || o.Query == nil || o.Encoder == nil
	invalidBatch := o.BatchRows <= 0 || o.BatchBytes == 0 || o.MaxRecordBytes < 128
	invalidPartitions := o.GroupBytes == 0 || o.PartitionMemoryBytes == 0 || o.WorkBytes == 0 || o.MaxPartitions < 2
	if missingInput || invalidBatch || invalidPartitions {
		return runtime.ErrInvalidConfig
	}
	if o.Phase > Final {
		return runtime.ErrInvalidConfig
	}
	for _, n := range []uint64{o.BatchBytes, o.MaxRecordBytes, o.GroupBytes, o.PartitionMemoryBytes, o.WorkBytes, uint64(o.MaxPartitions)} {
		if n > uint64(math.MaxInt)/128 {
			return runtime.ErrInvalidConfig
		}
	}
	if len(o.OutputLayouts) != len(o.GroupColumns)+len(o.Calls) {
		return columnar.ErrValueLayout
	}
	if len(o.GroupColumns) == 0 {
		o.OrderedGroups = true
	}
	if err := c.check(ctx); err != nil {
		return err
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.stopQuery = context.AfterFunc(o.Query.Context(), c.cancel)
	hasDirect := false
	for _, call := range o.Calls {
		hasDirect = hasDirect || len(call.DirectLayouts) != 0
	}
	directGroups := max(len(o.DirectGroupColumns), o.DirectGroupCount)
	if hasDirect && directGroups == 0 {
		directGroups = len(o.GroupColumns)
	}
	base := uint64(unsafe.Sizeof(cursor{})) + 4096
	base += uint64(len(o.InputLayouts)+len(o.OutputLayouts)+len(o.Calls))*1024 + uint64(len(o.GroupColumns)+directGroups)*256
	// Per-row scratch is bounded by the widest call and the widest direct-argument
	// list in the plan; it is admitted here because it is retained for the scan.
	maxArity, maxDirect := 0, 0
	for _, call := range o.Calls {
		maxArity = max(maxArity, len(call.Arguments))
		maxDirect = max(maxDirect, len(call.DirectLayouts))
		// Each compiled call owns arity-sized argument identity, and a declared
		// aggregate additionally retains its resolved support plan for the scan.
		base += uint64(len(call.Arguments))*uint64(unsafe.Sizeof(int(0))+unsafe.Sizeof(model.Type{})) + 256
	}
	base += uint64(maxArity+maxDirect) * uint64(unsafe.Sizeof(model.Value{}))
	for _, call := range o.Calls {
		if isJSONAggregate(call.Function) {
			base += 4096
		}
		if isRangeAggregate(call.Function) {
			base += 4096
		}
		if isScalarAggregate(call.Function) {
			base += uint64(unsafe.Sizeof(scalarAggregatePlan{}))
		}
		_, ordered := orderedSetOperation(call.Function)
		if ordered {
			base += uint64(len(call.DirectLayouts))*256 + uint64(unsafe.Sizeof(orderedRows{}))
		}
		c.hasOrderedSet = c.hasOrderedSet || ordered
	}
	maxOrder := 0
	for _, call := range o.Calls {
		orderBytes := uint64(len(call.OrderBy)) * 256
		if base > o.WorkBytes || orderBytes > o.WorkBytes-base {
			return resource("admit aggregate ordering", nil)
		}
		base += orderBytes
		maxOrder = max(maxOrder, len(call.OrderBy))
		c.hasOrder = c.hasOrder || len(call.OrderBy) != 0
	}
	extraColumns := 0
	if c.hasOrder {
		o.OrderedGroups = true
		base += 4096
		extraColumns = 3
	}
	if base > o.WorkBytes {
		return resource("admit aggregate schema", nil)
	}
	claim, err := o.Query.Allocator(runtime.WorkMemory).Reserve(base)
	if err != nil {
		return err
	}
	c.claim = claim
	c.layouts = make([]model.TypeLayout, len(o.InputLayouts), len(o.InputLayouts)+len(o.Calls)+extraColumns)
	outputs := make([]model.TypeLayout, len(o.OutputLayouts))
	remaining := base / 2
	for i, layout := range o.InputLayouts {
		owned, used, err := model.CopyTypeLayout(ctx, layout, remaining)
		if err != nil {
			return err
		}
		c.layouts[i], remaining = owned, remaining-used
	}
	for i, layout := range o.OutputLayouts {
		owned, used, err := model.CopyTypeLayout(ctx, layout, remaining)
		if err != nil {
			return err
		}
		outputs[i], remaining = owned, remaining-used
	}
	o.InputLayouts, o.OutputLayouts = c.layouts, outputs
	o.GroupColumns = append([]int(nil), o.GroupColumns...)
	if hasDirect {
		if o.DirectGroupCount == 0 {
			o.DirectGroupCount = len(o.GroupColumns)
		}
		if o.DirectGroupCount < 0 {
			return columnar.ErrValueLayout
		}
		if o.DirectGroupColumns == nil {
			o.DirectGroupColumns = make([]int, o.DirectGroupCount)
			for i := range o.DirectGroupColumns {
				o.DirectGroupColumns[i] = i
			}
		}
		if len(o.DirectGroupColumns) != o.DirectGroupCount {
			return columnar.ErrValueLayout
		}
	}
	for _, column := range o.DirectGroupColumns {
		if column < -1 || column >= len(o.GroupColumns) {
			return columnar.ErrValueLayout
		}
	}
	o.DirectGroupColumns = append([]int(nil), o.DirectGroupColumns...)
	c.groupKey = make([]storage.KeyColumn, len(o.GroupColumns))
	for i, column := range o.GroupColumns {
		if column < 0 || column >= len(c.layouts) {
			return columnar.ErrValueLayout
		}
		if !model.TypeLayoutsEqual(outputs[i], c.layouts[column]) {
			return columnar.ErrValueLayout
		}
		c.groupKey[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: c.layouts[column].Type, Children: c.layouts[column].Children, NullsFirst: c.partitioned != nil}
	}
	keyBytes, err := storage.KeyLayoutBytes(ctx, c.groupKey, remaining)
	if err != nil {
		return err
	}
	c.groupKey, _, err = storage.CopyKeyLayout(ctx, c.groupKey, keyBytes)
	if err != nil {
		return err
	}
	remaining -= keyBytes
	if _, err := o.Encoder.EncodePrefix(ctx, c.groupKey, nil, o.MaxRecordBytes); err != nil {
		return err
	}
	c.calls = make([]compiledCall, len(o.Calls))
	for i, call := range o.Calls {
		compiled, err := compileCall(ctx, call, outputs[len(o.GroupColumns)+i], c.layouts, o)
		if err != nil {
			return err
		}
		if compiled.directCount != 0 && compiled.directEvaluator == nil {
			return runtime.ErrInvalidConfig
		}
		for _, key := range []*[]storage.KeyColumn{&compiled.key, &compiled.orderKey} {
			owned, used, err := storage.CopyKeyLayout(ctx, *key, remaining)
			if err != nil {
				return err
			}
			*key, remaining = owned, remaining-used
		}
		c.calls[i] = compiled
		c.hasDistinct = c.hasDistinct || call.Distinct
		// A zero-argument aggregate such as count(*) has no input type to compare.
		if compiled.arity != 0 {
			c.hasNumeric = c.hasNumeric || compiled.op.numericArithmetic() && (compiled.output.ID == 1700 || compiled.inputs[0].ID == 1700)
		}
	}
	o.Calls = nil
	o.Builtin = builtin.ResolveOptions{}
	c.perGroup = 256 + o.MaxRecordBytes*9 + uint64(len(c.calls))*(uint64(unsafe.Sizeof(transition{}))+o.MaxRecordBytes*2) + uint64(len(o.GroupColumns))*uint64(unsafe.Sizeof(model.Value{}))
	// Scalar bit transitions retain one M-byte value and allocate at most one
	// M-byte replacement; both fit the per-call allowance above.
	for _, call := range c.calls {
		if call.op == jsonAggregate {
			c.perGroup += o.MaxRecordBytes * 8
		}
		if call.op.rangeAggregation() {
			c.perGroup += o.MaxRecordBytes * 6
		}
		if call.op.arrayAggregation() {
			// Array state retains at most 8M; the existing call allowance is 2M.
			// The shared 32M scratch admits overlapping growth and active copies.
			c.perGroup += o.MaxRecordBytes * 6
		}
		if call.op == stringAggregate && call.output.Storage == model.TypeString {
			// The state uses M bytes; owned text finalization may round its
			// allocation up to 2M. Growth overlap uses the shared scratch.
			c.perGroup += o.MaxRecordBytes
		}
		if call.op.pairedMoments() {
			c.perGroup += uint64(unsafe.Sizeof(pairedMoments{}))
		}
		if call.op.statistical() && call.output.ID == 1700 {
			extra := numericStatisticsBytes(o.MaxRecordBytes)
			if c.perGroup > o.GroupBytes || extra > o.GroupBytes-c.perGroup {
				return resource("numeric statistical state exceeds group workspace", nil)
			}
			c.perGroup += extra
		}
	}
	if c.perGroup > o.GroupBytes {
		return resource("aggregate group exceeds group workspace", nil)
	}
	c.maxGroups = int(o.GroupBytes / c.perGroup)
	c.metadata = base + 4096
	// Numeric kernels cap coefficients at 32768 digits; their temporary big.Int
	// operands and decimal formatting are admitted independently of result bytes.
	scratch := o.MaxRecordBytes * 32
	if c.hasNumeric {
		scratch += max(4<<20, builtin.NumericMomentsWorkBytes)
	}
	fixed := base + o.GroupBytes + scratch
	if o.OrderedGroups {
		fixed += c.perGroup
	}
	spoolWork := c.metadata + o.MaxRecordBytes*9 + 8192
	spoolPeak := spoolWork*2 + (o.MaxRecordBytes*9+8192)*2 + o.PartitionMemoryBytes*2
	if c.hasOrderedSet {
		spoolPeak += (spoolWork + o.PartitionMemoryBytes) * 2
	}
	if c.hasDistinct || c.hasOrder {
		c.sortWork = o.WorkBytes / 2
		spoolPeak += o.BatchBytes
	}
	if fixed > o.WorkBytes || spoolPeak > o.WorkBytes-fixed || c.sortWork > o.WorkBytes-fixed-spoolPeak {
		return resource("aggregate work limit cannot admit partitions", nil)
	}
	if err := claim.Resize(fixed); err != nil {
		return err
	}
	if !o.OrderedGroups {
		c.groups = make([]*group, 0, c.maxGroups)
		c.lookup = make(map[string]*group, c.maxGroups)
	}
	// Per-row scratch is allocated once for the widest call in the plan.
	c.arguments = make([]model.Value, maxArity)
	c.directValues = make([]model.Value, maxDirect)
	c.keyValues = make([]model.Value, max(len(c.groupKey)+2, maxOrder))
	c.output = make([]model.Value, len(outputs))
	c.row = make([]model.Value, len(c.layouts)+len(c.calls)+extraColumns)
	if c.hasNumeric {
		c.numericAdd, err = builtin.Resolve(1724, builtin.ResolveOptions{})
		if err != nil {
			return err
		}
		c.numericDivide, err = builtin.Resolve(1727, builtin.ResolveOptions{})
		if err != nil {
			return err
		}
		c.numericTypmod, err = builtin.Resolve(1703, builtin.ResolveOptions{})
		if err != nil {
			return err
		}
	}
	for _, call := range c.calls {
		if call.op == percentileContinuous && call.inputs[0].ID == 1186 {
			c.intervalSubtract, err = builtin.Resolve(1170, o.Builtin)
			if err != nil {
				return err
			}
			c.intervalMultiply, err = builtin.Resolve(1618, o.Builtin)
			if err != nil {
				return err
			}
			c.intervalAdd, err = builtin.Resolve(1169, o.Builtin)
			if err != nil {
				return err
			}
			break
		}
	}
	c.builder, err = columnar.NewBuilder(outputs, columnar.BuilderOptions{MaxRows: o.BatchRows, MaxBytes: o.BatchBytes, Allocator: o.Query.Allocator(runtime.OutputMemory)})
	return err
}

func decomposableOperation(function model.FunctionID) (operation, bool) {
	switch function {
	case 2147, 2803:
		return count, true
	case 2107, 2108, 2109, 2110, 2111, 2113, 2114:
		return sum, true
	default:
		return 0, false
	}
}

func validCombine(function model.FunctionID, state model.Type) bool {
	signature, ok := builtin.LookupSignature(function)
	return ok && signature.Kind == 'f' && signature.Volatility == 'i' && signature.Strict &&
		signature.ArgumentCount == 2 && signature.Arguments[0] == state.ID &&
		signature.Arguments[1] == state.ID && signature.Result == state.ID
}

func compileFinalCall(call Call, outputLayout model.TypeLayout, layouts []model.TypeLayout) (compiledCall, error) {
	result := newCompiledCall(1, outputLayout.Type)
	result.combining = true
	unsupported := func(reason string) (compiledCall, error) {
		return result, &UnsupportedError{Function: call.Function, Reason: reason}
	}
	if call.Distinct || call.FilterColumn != nil || len(call.OrderBy) != 0 || len(call.DirectLayouts) != 0 || call.DirectEvaluator != nil {
		return unsupported("final phase carries row-level aggregate modifiers")
	}
	operation, supported := decomposableOperation(call.Function)
	if !supported || !validCombine(call.Combine, outputLayout.Type) {
		return unsupported("combine contract")
	}
	if len(call.Arguments) != 1 || call.Arguments[0] < 0 || call.Arguments[0] >= len(layouts) || !model.TypeLayoutsEqual(layouts[call.Arguments[0]], outputLayout) {
		return result, columnar.ErrValueLayout
	}
	signature, ok := builtin.LookupSignature(call.Function)
	if !ok || signature.Kind != 'a' || signature.Result != outputLayout.Type.ID {
		return unsupported("not a pinned aggregate signature")
	}
	result.op = operation
	result.arguments[0] = call.Arguments[0]
	result.inputs[0] = layouts[call.Arguments[0]].Type
	return result, nil
}

func compileCall(ctx context.Context, call Call, outputLayout model.TypeLayout, layouts []model.TypeLayout, options *Options) (compiledCall, error) {
	if options.Phase == Final {
		return compileFinalCall(call, outputLayout, layouts)
	}
	if ordered, handled, err := compileOrderedSetCall(ctx, call, outputLayout, layouts, options); handled {
		return ordered, err
	}
	output := outputLayout.Type
	result := newCompiledCall(len(call.Arguments), output)
	result.distinct = call.Distinct
	unsupported := func(reason string) (compiledCall, error) {
		return result, &UnsupportedError{Function: call.Function, Reason: reason}
	}
	if len(call.DirectLayouts) != 0 || call.DirectEvaluator != nil {
		return unsupported("direct arguments")
	}
	sig, ok := builtin.LookupSignature(call.Function)
	if !ok || sig.Kind != 'a' {
		// A SQL-defined aggregate has no pinned signature; its declaration is
		// the executable contract, so it is compiled from the resolver.
		if options.Aggregates != nil {
			return compileUserAggregate(ctx, call.Function, call, outputLayout, layouts, options)
		}
		return unsupported("not a pinned aggregate signature")
	}
	arrayExtrema := call.Function == 2050 || call.Function == 2051
	recordExtrema := call.Function == 6373 || call.Function == 6374
	polymorphic := call.Function == 6291 || call.Function == 2335 || call.Function == 4053 || arrayExtrema || recordExtrema || isRangeAggregate(call.Function)
	if len(call.Arguments) != int(sig.ArgumentCount) || !polymorphic && output.ID != sig.Result {
		return result, columnar.ErrValueLayout
	}
	for i, column := range call.Arguments {
		if column < 0 || column >= len(layouts) {
			return result, columnar.ErrValueLayout
		}
		result.arguments[i], result.inputs[i] = column, layouts[column].Type
		if sig.Arguments[i] != 2276 && !(isJSONAggregate(call.Function) && sig.Arguments[i] == 2283) && !polymorphic && result.inputs[i].ID != sig.Arguments[i] {
			return result, columnar.ErrValueLayout
		}
	}
	if call.FilterColumn != nil {
		column := *call.FilterColumn
		if column < 0 || column >= len(layouts) || layouts[column].Type.ID != 16 || layouts[column].Type.Storage != model.TypeBoolean {
			return result, columnar.ErrValueLayout
		}
		result.filter = column
	}
	switch call.Function {
	case 4301, 4389, 4450, 6227:
		result.op = rangeUnionAggregate
		if call.Function == 4389 || call.Function == 4450 {
			result.op = rangeIntersectAggregate
		}
		var err error
		result.rangePlan, err = compileRangeAggregate(ctx, call.Function, layouts[call.Arguments[0]], outputLayout, options.Builtin)
		if err != nil {
			return result, err
		}
	case 3175, 3197, 3267, 3270, 6276, 6280, 6281, 6282, 6284, 6288, 6289, 6290:
		result.op = jsonAggregate
		var err error
		result.json, err = compileJSONAggregate(ctx, call, layouts, options.Builtin)
		if err != nil {
			return result, err
		}
	case 2147, 2803:
		result.op = count
	case 2100, 2101, 2102, 2103, 2104, 2105, 2106:
		result.op = average
	case 2107, 2108, 2109, 2110, 2111, 2112, 2113, 2114:
		result.op = sum
	case 2517, 2519, 2236, 2238, 2240, 2242:
		result.op = bitwiseAnd
	case 2518, 2237, 2239, 2241, 2243:
		result.op = bitwiseOr
	case 6164, 6165, 6166, 6167:
		result.op = bitwiseXor
	case 2718, 2719, 2720, 2721, 2722, 2723:
		result.op = variancePopulation
	case 2148, 2149, 2150, 2151, 2152, 2153, 2641, 2642, 2643, 2644, 2645, 2646:
		result.op = varianceSample
	case 2724, 2725, 2726, 2727, 2728, 2729:
		result.op = stddevPopulation
	case 2154, 2155, 2156, 2157, 2158, 2159, 2712, 2713, 2714, 2715, 2716, 2717:
		result.op = stddevSample
	case 2818:
		result.op = regrCount
	case 2819:
		result.op = regrSxx
	case 2820:
		result.op = regrSyy
	case 2821:
		result.op = regrSxy
	case 2822:
		result.op = regrAvgx
	case 2823:
		result.op = regrAvgy
	case 2824:
		result.op = regrR2
	case 2825:
		result.op = regrSlope
	case 2826:
		result.op = regrIntercept
	case 2827:
		result.op = covarPopulation
	case 2828:
		result.op = covarSample
	case 2829:
		result.op = correlation
	case 3538, 3545:
		result.op = stringAggregate
	case 2335, 4053:
		result.op = arrayAggregate
		if call.Function == 4053 {
			result.op = arrayAggregateArrays
		}
		if err := validateArrayAggregateLayouts(result.op, layouts[call.Arguments[0]], outputLayout); err != nil {
			return result, err
		}
	case 6291:
		result.op = anyValue
		inputLayout := layouts[call.Arguments[0]]
		if err := inputLayout.Validate(); err != nil {
			return result, err
		}
		if output.Modifier == -1 {
			inputLayout.Type.Modifier = -1
		}
		if !model.TypeLayoutsEqual(inputLayout, outputLayout) {
			return result, columnar.ErrValueLayout
		}
	default:
		if sig.Name != "min" && sig.Name != "max" {
			return unsupported("transition family")
		}
		if sig.ArgumentCount != 1 || sig.Result != sig.Arguments[0] {
			return unsupported("polymorphic extrema")
		}
		if arrayExtrema || recordExtrema {
			input := layouts[call.Arguments[0]]
			if err := input.Validate(); err != nil {
				return result, err
			}
			if err := outputLayout.Validate(); err != nil {
				return result, err
			}
			if output.Modifier == -1 {
				input.Type.Modifier = -1
			}
			storage := model.TypeArray
			if recordExtrema {
				storage = model.TypeComposite
			}
			if input.Type.Storage != storage || !model.TypeLayoutsEqual(input, outputLayout) {
				return result, columnar.ErrValueLayout
			}
		}
		result.op = minimum
		if sig.Name == "max" {
			result.op = maximum
		}
	}
	if options.Phase == Partial {
		operation, supported := decomposableOperation(call.Function)
		modified := call.Distinct || call.FilterColumn != nil || len(call.OrderBy) != 0 || len(call.DirectLayouts) != 0 || call.DirectEvaluator != nil
		if modified || !supported || operation != result.op || !validCombine(call.Combine, output) {
			return unsupported("combine contract")
		}
	} else if call.Combine != 0 {
		return unsupported("combine contract on complete phase")
	}
	if err := applyAggregateCallModifiers(ctx, call, &result, layouts, options); err != nil {
		return result, err
	}
	// A zero-argument aggregate such as count(*) has no input types to inspect.
	// Every branch below that reads an input either cannot be reached with arity 0
	// or overrides wantStorage before using it, so the output storage is the only
	// meaningful expectation there.
	wantStorage := output.Storage
	if result.arity != 0 {
		wantStorage = result.inputs[0].Storage
	}
	if result.op == jsonAggregate {
		wantStorage = model.TypeJSONB
	}
	if result.op.arrayAggregation() {
		wantStorage = model.TypeArray
	}
	if result.op == stringAggregate {
		wantStorage = model.TypeString
		if call.Function == 3545 {
			wantStorage = model.TypeBytes
		}
		for i := range result.arity {
			if result.inputs[i].Storage != wantStorage {
				return result, columnar.ErrValueLayout
			}
		}
	}
	if result.op == count || result.op == regrCount {
		wantStorage = model.TypeInt64
	}
	if result.op == bitwiseAnd || result.op == bitwiseOr || result.op == bitwiseXor {
		wantStorage = model.TypeInt64
		if result.inputs[0].ID == 16 {
			wantStorage = model.TypeBoolean
		}
		if result.inputs[0].ID == 1560 {
			wantStorage = model.TypeBytes
		}
		if result.inputs[0].Storage != wantStorage {
			return result, columnar.ErrValueLayout
		}
	}
	if result.op == sum || result.op == average || result.op.statistical() {
		switch output.ID {
		case 20, 790:
			wantStorage = model.TypeInt64
		case 700, 701:
			wantStorage = model.TypeFloat64
		case 1700:
			wantStorage = model.TypeNumeric
		}
		inputStorage := model.TypeInt64
		if result.inputs[0].ID == 700 || result.inputs[0].ID == 701 {
			inputStorage = model.TypeFloat64
		}
		if result.inputs[0].ID == 1700 {
			inputStorage = model.TypeNumeric
		}
		if result.inputs[0].ID == 1186 {
			inputStorage = model.TypeInterval
		}
		if result.inputs[0].Storage != inputStorage {
			return result, columnar.ErrValueLayout
		}
	}
	if result.op >= regrCount && result.op <= correlation {
		for i := range result.arity {
			if result.inputs[i].Storage != model.TypeFloat64 {
				return result, columnar.ErrValueLayout
			}
		}
	}
	if output.Storage != wantStorage {
		return result, columnar.ErrValueLayout
	}
	if isScalarAggregate(call.Function) {
		var err error
		result.scalar, err = compileScalarAggregate(call.Function)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

// applyAggregateCallModifiers wires the row-level modifiers a compiled call
// shares regardless of which operation implements it: DISTINCT's qualification
// key, the sort key a declared minimum/maximum compares with, and the ORDER BY
// sort specification. DISTINCT is admitted whenever the call has arguments, so
// the count(*) spelling is the only rejection. Every other operation consumes
// these fields identically, so a SQL-defined aggregate reaches the same
// machinery as a pinned one instead of silently ignoring its modifiers.
func applyAggregateCallModifiers(ctx context.Context, call Call, result *compiledCall, layouts []model.TypeLayout, options *Options) error {
	if call.Distinct && result.arity == 0 {
		return &UnsupportedError{Function: call.Function, Reason: "DISTINCT count-star"}
	}
	result.distinct = call.Distinct
	if result.op == minimum || result.op == maximum || call.Distinct {
		result.key = make([]storage.KeyColumn, result.arity)
		collation := builtin.InputCollation(call.InputCollation, result.inputs[:result.arity])
		for i := range result.arity {
			typ := result.inputs[i]
			if builtin.TypeCollatable(typ) {
				typ.Collation = collation
			}
			result.key[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: typ, Children: layouts[result.arguments[i]].Children}
		}
		if call.Distinct || !builtin.TypeCollatable(result.key[0].Type) || collation != 0 {
			if _, err := options.Encoder.EncodePrefix(ctx, result.key, nil, options.MaxRecordBytes); err != nil {
				return err
			}
		}
	}
	if len(call.OrderBy) != 0 {
		result.order = append([]OrderKey(nil), call.OrderBy...)
		result.orderKey = make([]storage.KeyColumn, len(call.OrderBy))
		for i, key := range call.OrderBy {
			if key.Column < 0 || key.Column >= len(layouts) {
				return columnar.ErrValueLayout
			}
			result.orderKey[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: layouts[key.Column].Type, Children: layouts[key.Column].Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
		}
		if _, err := options.Encoder.EncodePrefix(ctx, result.orderKey, nil, options.MaxRecordBytes); err != nil {
			return err
		}
	}
	return nil
}

func (op operation) arrayAggregation() bool {
	return op == arrayAggregate || op == arrayAggregateArrays
}

func (op operation) numericArithmetic() bool {
	return !op.orderedSet() && op != anyValue && op != jsonAggregate && !op.arrayAggregation() && !op.rangeAggregation()
}

// CheckSupport checks aggregate semantics without admitting execution resources.
func CheckSupport(ctx context.Context, options Options) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if options.Encoder == nil || len(options.OutputLayouts) != len(options.GroupColumns)+len(options.Calls) {
		return runtime.ErrInvalidConfig
	}
	hasDirect := false
	for _, call := range options.Calls {
		hasDirect = hasDirect || len(call.DirectLayouts) != 0
	}
	if hasDirect {
		if options.DirectGroupCount == 0 {
			options.DirectGroupCount = len(options.GroupColumns)
		}
		if options.DirectGroupCount < 0 {
			return columnar.ErrValueLayout
		}
		if options.DirectGroupColumns == nil {
			options.DirectGroupColumns = make([]int, options.DirectGroupCount)
			for i := range options.DirectGroupColumns {
				options.DirectGroupColumns[i] = i
			}
		}
		if len(options.DirectGroupColumns) != options.DirectGroupCount {
			return columnar.ErrValueLayout
		}
	}
	for _, column := range options.DirectGroupColumns {
		if column < -1 || column >= len(options.GroupColumns) {
			return columnar.ErrValueLayout
		}
	}
	for i, column := range options.GroupColumns {
		if column < 0 || column >= len(options.InputLayouts) || options.OutputLayouts[i].Type != options.InputLayouts[column].Type {
			return columnar.ErrValueLayout
		}
	}
	for i, call := range options.Calls {
		if _, err := compileCall(ctx, call, options.OutputLayouts[len(options.GroupColumns)+i], options.InputLayouts, &options); err != nil {
			return err
		}
	}
	return nil
}
