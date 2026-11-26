package neosepia

// Private NeoSepia predicate kernel for storage pushdown.
//
// The common storage.Predicate carries resolved operator, type and collation
// identities. This file compiles an accepted predicate into an immutable
// predicateProgram and evaluates it against columnar.Batch views using native
// typed vector encodings. It performs no source-to-source query conversion.
//
// Supported resolved semantics (all-or-nothing; anything else stays residual):
//
//   - Integer comparisons over int2/int4/int8 in any resolved pairing, using
//     the exact pinned pg_operator.dat OIDs below. Operands compare as int64,
//     which is exact for every int2/int4/int8 value.
//   - Boolean comparisons (=, <>, <, >, <=, >=) with false < true.
//   - float4/float8 comparisons in any resolved pairing with PostgreSQL NaN
//     ordering: NaN is greater than every non-NaN value and equal only to NaN.
//     Operands decode as float64 regardless of the SQL width.
//   - timestamp and timestamptz same-type comparisons as int64 order. Cross
//     timestamp/timestamptz operators are declined: their conversions belong
//     to the type layer, not to a guessed storage comparator.
//   - text/varchar comparisons with the pinned text operators and a binary
//     C/POSIX collation (950 C, 951 POSIX).
//     pg_operator.dat defines text-only comparison operators; PostgreSQL
//     resolves varchar comparisons to these same operators with bytewise
//     semantics, so operand IDs 25 (text) and 1043 (varchar) are accepted
//     interchangeably. bpchar blank-padding and name semantics are declined.
//   - IS NULL / IS NOT NULL over any bound column type.
//   - IN / NOT IN with SQL three-valued semantics, including NULL members,
//     NULL probes and empty member lists.
//   - Boolean columns/constants and AND/OR with Kleene three-valued logic.
//
// Pinned sources: testdata/postgres184/catalog/pg_operator.dat for every
// operator OID below, pg_type.dat for type OIDs (bool 16, int8 20, int2 21,
// int4 23, text 25, float4 700, float8 701, varchar 1043, timestamp 1114,
// timestamptz 1184) and pg_collation.dat for collation OIDs (100 default,
// 950 C, 951 POSIX; 811/962/963/6411 are locale-provider collations and are
// rejected).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	errPredicateBudget        = errors.New("neosepia predicate budget exceeded")
	errPredicateScratchClosed = errors.New("neosepia predicate scratch closed")
)

// Resolved SQL type identities accepted by the kernel.
const (
	predTypeBool        model.TypeID = 16
	predTypeInt8        model.TypeID = 20
	predTypeInt2        model.TypeID = 21
	predTypeInt4        model.TypeID = 23
	predTypeText        model.TypeID = 25
	predTypeFloat4      model.TypeID = 700
	predTypeFloat8      model.TypeID = 701
	predTypeVarchar     model.TypeID = 1043
	predTypeTimestamp   model.TypeID = 1114
	predTypeTimestamptz model.TypeID = 1184
)

// Binary collations with C/POSIX bytewise semantics. Zero is an unspecified
// collation, which the binder uses for non-collatable or collation-free
// comparisons.
const (
	predCollNone    model.CollationID = 0
	predCollDefault model.CollationID = 100
	predCollC       model.CollationID = 950
	predCollPOSIX   model.CollationID = 951
)

type predCmpOp uint8

const (
	predEq predCmpOp = iota
	predNe
	predLt
	predGt
	predLe
	predGe
)

type predFamily uint8

const (
	predInt predFamily = iota
	predBool
	predFloat
	predText
	predTimestamp
)

type predOpSig struct {
	left   model.TypeID
	right  model.TypeID
	op     predCmpOp
	family predFamily
}

// pinnedPredOps maps exact pg_operator.dat OIDs to their resolved operand
// types. Entries are grouped by SQL type pair; each OID row is
// (=, <>, <, >, <=, >=).
var pinnedPredOps = buildPinnedPredOps()

func buildPinnedPredOps() map[model.OperatorID]predOpSig {
	out := make(map[model.OperatorID]predOpSig)
	ops := []predCmpOp{predEq, predNe, predLt, predGt, predLe, predGe}
	add := func(family predFamily, left, right model.TypeID, oids ...uint32) {
		for i, oid := range oids {
			out[model.OperatorID(oid)] = predOpSig{left: left, right: right, op: ops[i], family: family}
		}
	}
	add(predInt, predTypeInt2, predTypeInt2, 94, 519, 95, 520, 522, 524)
	add(predInt, predTypeInt4, predTypeInt4, 96, 518, 97, 521, 523, 525)
	add(predInt, predTypeInt8, predTypeInt8, 410, 411, 412, 413, 414, 415)
	add(predInt, predTypeInt2, predTypeInt4, 532, 538, 534, 536, 540, 542)
	add(predInt, predTypeInt4, predTypeInt2, 533, 539, 535, 537, 541, 543)
	add(predInt, predTypeInt2, predTypeInt8, 1862, 1863, 1864, 1865, 1866, 1867)
	add(predInt, predTypeInt8, predTypeInt2, 1868, 1869, 1870, 1871, 1872, 1873)
	add(predInt, predTypeInt4, predTypeInt8, 15, 36, 37, 76, 80, 82)
	add(predInt, predTypeInt8, predTypeInt4, 416, 417, 418, 419, 420, 430)
	add(predBool, predTypeBool, predTypeBool, 91, 85, 58, 59, 1694, 1695)
	add(predFloat, predTypeFloat4, predTypeFloat4, 620, 621, 622, 623, 624, 625)
	add(predFloat, predTypeFloat8, predTypeFloat8, 670, 671, 672, 674, 673, 675)
	add(predFloat, predTypeFloat4, predTypeFloat8, 1120, 1121, 1122, 1123, 1124, 1125)
	add(predFloat, predTypeFloat8, predTypeFloat4, 1130, 1131, 1132, 1133, 1134, 1135)
	add(predText, predTypeText, predTypeText, 98, 531, 664, 666, 665, 667)
	add(predTimestamp, predTypeTimestamp, predTypeTimestamp, 2060, 2061, 2062, 2064, 2063, 2065)
	add(predTimestamp, predTypeTimestamptz, predTypeTimestamptz, 1320, 1321, 1322, 1324, 1323, 1325)
	return out
}

func predTextType(id model.TypeID) bool { return id == predTypeText || id == predTypeVarchar }

func predTextCollation(id model.CollationID) bool {
	return id == predCollC || id == predCollPOSIX
}

func predFamilyStorage(family predFamily) model.DataType {
	switch family {
	case predInt:
		return model.TypeInt64
	case predBool:
		return model.TypeBoolean
	case predFloat:
		return model.TypeFloat64
	case predText:
		return model.TypeString
	case predTimestamp:
		return model.TypeTimestamp
	}
	return model.TypeAny
}

type predColKind uint8

const (
	predNullOnly predColKind = iota
	predColInt
	predColFloat
	predColText
)

type predNodeKind uint8

const (
	predScalarColumn predNodeKind = iota
	predScalarConst
	predBoolColumn
	predBoolConst
	predCompare
	predNullTest
	predIn
	predAnd
	predOr
)

// predScalarRef addresses one comparison/IN operand. Exactly one of col and
// cnst is non-negative: col indexes program column storage, cnst indexes
// program constants.
type predScalarRef struct {
	col  int32
	cnst int32
}

type predCompiledNode struct {
	kind      predNodeKind
	col       int32
	cnst      int32
	bval      bool
	bnull     bool
	op        predCmpOp
	family    predFamily
	left      predScalarRef
	right     predScalarRef
	members   []predScalarRef
	negated   bool
	a, b      uint32
	boolChild bool
}

// predicateProgram is immutable after compilePredicate returns and is safe
// for concurrent Filter calls with distinct scratch instances.
type predicateProgram struct {
	cols        []model.ColumnID
	colTypes    []model.Type
	colKinds    []predColKind
	consts      []model.Value
	nodes       []predCompiledNode
	root        uint32
	boolCount   int
	trivialTrue bool
}

// predicateScratch carries per-invocation evaluation buffers. The zero value
// is ready for use. Buffers borrow batch payloads until the next Filter or
// Close; Close releases every reference.
type predicateScratch struct {
	closed bool
	sel    []uint32
	empty  []uint32
	xnull  [][]byte
	xint   [][]int64
	xfloat [][]float64
	xoff   [][]uint32
	xlen   [][]uint32
	xdata  [][]byte
	xvalid []bool
	pool   [][]byte
}

// Close releases every borrowed reference and pooled buffer.
func (s *predicateScratch) Close() error {
	*s = predicateScratch{closed: true}
	return nil
}

// Columns returns the immutable sorted distinct column IDs the program reads.
// Filter input columns must arrive in exactly this order.
func (p *predicateProgram) Columns() []model.ColumnID {
	return p.cols
}

func predInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", storage.ErrInvalidPredicate, fmt.Sprintf(format, args...))
}

// compilePredicate compiles the reachable predicate subtree into an immutable
// program. It returns (nil, false, nil) when every reachable reference is
// valid but some operator, type or collation has no pinned exact semantics.
// Invalid bound column or type references return an error.
// Constant payloads are borrowed through the program's lifetime. The bound plan
// must own or retain them before compilation; compilation does not clone values.
func compilePredicate(ctx context.Context, predicate storage.Predicate, layout []storage.ColumnLayout, maxNodes int, maxBytes uint64) (*predicateProgram, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if maxNodes <= 0 || maxBytes < 256 || uint64(len(layout)) > (maxBytes-256)/256 {
		return nil, false, errPredicateBudget
	}
	available := maxBytes - 256 - uint64(len(layout))*256
	if predicate.Empty() {
		return &predicateProgram{trivialTrue: true}, true, nil
	}
	byID := make(map[model.ColumnID]int, len(layout))
	for i, col := range layout {
		byID[col.ID] = i
	}
	// Walk the reachable DAG from the root; unreachable nodes in a shared
	// Subtree are ignored entirely.
	reachable, err := predReachable(ctx, predicate, min(maxNodes, int(min(available/2048, uint64(math.MaxInt)))))
	if err != nil {
		return nil, false, err
	}
	if len(reachable) > maxNodes {
		return nil, false, fmt.Errorf("%w: %d reachable nodes exceed %d", errPredicateBudget, len(reachable), maxNodes)
	}
	prog := &predicateProgram{}
	colUsed := make(map[model.ColumnID]bool)
	// Validate every reachable node before accepting anything.
	for _, idx := range reachable {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		n := predicate.Node(idx)
		switch n.Kind {
		case storage.PredicateColumn:
			li, ok := byID[n.ColumnID]
			if !ok {
				return nil, false, predInvalid("column %d not in bound layout", uint32(n.ColumnID))
			}
			if layout[li].Type != n.Type {
				return nil, false, predInvalid("column %d storage %d mismatches layout storage %d", uint32(n.ColumnID), n.Type.Storage, layout[li].Type.Storage)
			}
			colUsed[n.ColumnID] = true
		case storage.PredicateConstant:
			if n.Type.ID == predTypeBool && n.Type.Storage == model.TypeBoolean {
				c := predicate.Constant(n.ConstantIndex)
				if !c.IsNull && c.IntVal != 0 && c.IntVal != 1 {
					return nil, false, predInvalid("boolean constant %d out of range", c.IntVal)
				}
			}
		case storage.PredicateComparison:
			ok, err := predCheckComparison(predicate, n)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				return nil, false, nil
			}
		case storage.PredicateNullTest:
			operand := predicate.Node(n.Left)
			if !predBooleanType(operand.Type) {
				if operand.Kind != storage.PredicateColumn && operand.Kind != storage.PredicateConstant {
					return nil, false, nil
				}
			}
		case storage.PredicateIn:
			ok, err := predCheckIn(predicate, n)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				return nil, false, nil
			}
		case storage.PredicateAnd, storage.PredicateOr:
			// Children are boolean by construction; nothing further to prove.
		default:
			return nil, false, nil
		}
	}
	// Charge the immutable program against the compile budget.
	var references uint64
	for _, idx := range reachable {
		references += uint64(predicate.Node(idx).ArgumentCount)
	}
	progBytes := uint64(len(reachable))*2048 + references*256 + uint64(len(layout))*256 + 256
	if progBytes > maxBytes {
		return nil, false, fmt.Errorf("%w: program needs %d of %d bytes", errPredicateBudget, progBytes, maxBytes)
	}
	// Sorted distinct required columns.
	for id := range colUsed {
		prog.cols = append(prog.cols, id)
	}
	slices.Sort(prog.cols)
	prog.colTypes = make([]model.Type, len(prog.cols))
	prog.colKinds = make([]predColKind, len(prog.cols))
	colPos := make(map[model.ColumnID]int32, len(prog.cols))
	for i, id := range prog.cols {
		prog.colTypes[i] = layout[byID[id]].Type
		colPos[id] = int32(i)
	}
	// Determine per-column extraction kinds from scalar/boolean use.
	for _, idx := range reachable {
		n := predicate.Node(idx)
		switch n.Kind {
		case storage.PredicateColumn:
			if predBooleanType(n.Type) {
				prog.colKinds[colPos[n.ColumnID]] = predColInt
			}
		case storage.PredicateComparison:
			predUpgradeOperand(predicate.Node(n.Left), colPos, prog.colKinds)
			predUpgradeOperand(predicate.Node(n.Right), colPos, prog.colKinds)
		case storage.PredicateIn:
			predUpgradeOperand(predicate.Node(n.Left), colPos, prog.colKinds)
			for k := uint32(0); k < n.ArgumentCount; k++ {
				arg := predicate.Node(predicate.Argument(n.FirstArgument + k))
				predUpgradeOperand(arg, colPos, prog.colKinds)
			}
		}
	}
	// Compile reachable nodes in ascending (topological) order.
	PosOf := make(map[uint32]uint32, len(reachable))
	for _, idx := range reachable {
		PosOf[idx] = uint32(len(prog.nodes))
		prog.nodes = append(prog.nodes, predCompiledNode{})
		cn := &prog.nodes[len(prog.nodes)-1]
		n := predicate.Node(idx)
		switch n.Kind {
		case storage.PredicateColumn:
			cn.col = colPos[n.ColumnID]
			if predBooleanType(n.Type) {
				cn.kind = predBoolColumn
			} else {
				cn.kind = predScalarColumn
			}
		case storage.PredicateConstant:
			cn.cnst = predInternConst(prog, predicate.Constant(n.ConstantIndex))
			if predBooleanType(n.Type) {
				cn.kind = predBoolConst
				c := predicate.Constant(n.ConstantIndex)
				cn.bnull = c.IsNull
				cn.bval = !c.IsNull && c.IntVal == 1
			} else {
				cn.kind = predScalarConst
			}
		case storage.PredicateComparison:
			sig := pinnedPredOps[n.Operator]
			cn.kind = predCompare
			cn.op = sig.op
			cn.family = sig.family
			cn.left = predScalarOperand(predicate.Node(n.Left), predicate, colPos, prog)
			cn.right = predScalarOperand(predicate.Node(n.Right), predicate, colPos, prog)
		case storage.PredicateNullTest:
			cn.kind = predNullTest
			cn.negated = n.Negated
			operand := predicate.Node(n.Left)
			if predBooleanType(operand.Type) {
				cn.boolChild = true
				cn.a = PosOf[n.Left]
			} else {
				cn.left = predScalarOperand(operand, predicate, colPos, prog)
			}
		case storage.PredicateIn:
			cn.kind = predIn
			cn.negated = n.Negated
			cn.left = predScalarOperand(predicate.Node(n.Left), predicate, colPos, prog)
			for k := uint32(0); k < n.ArgumentCount; k++ {
				arg := predicate.Node(predicate.Argument(n.FirstArgument + k))
				cn.members = append(cn.members, predScalarOperand(arg, predicate, colPos, prog))
			}
		case storage.PredicateAnd:
			cn.kind = predAnd
			cn.a, cn.b = PosOf[n.Left], PosOf[n.Right]
		case storage.PredicateOr:
			cn.kind = predOr
			cn.a, cn.b = PosOf[n.Left], PosOf[n.Right]
		}
	}
	prog.root = PosOf[predicate.Root()]
	for _, cn := range prog.nodes {
		switch cn.kind {
		case predBoolColumn, predBoolConst, predCompare, predNullTest, predIn, predAnd, predOr:
			prog.boolCount++
		}
	}
	return prog, true, nil
}

func predBooleanType(t model.Type) bool {
	return t.ID == predTypeBool && t.Storage == model.TypeBoolean
}

// predReachable returns reachable node indices in ascending order.
func predReachable(ctx context.Context, p storage.Predicate, limit int) ([]uint32, error) {
	if limit <= 0 {
		return nil, errPredicateBudget
	}
	seen := make(map[uint32]bool)
	stack := []uint32{p.Root()}
	for len(stack) > 0 {
		if len(seen)%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		idx := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[idx] {
			continue
		}
		if uint64(idx) >= uint64(p.NodeCount()) {
			return nil, predInvalid("node %d out of range", idx)
		}
		if len(seen) >= limit {
			return nil, errPredicateBudget
		}
		seen[idx] = true
		n := p.Node(idx)
		switch n.Kind {
		case storage.PredicateComparison, storage.PredicateAnd, storage.PredicateOr:
			stack = append(stack, n.Left, n.Right)
		case storage.PredicateNullTest:
			stack = append(stack, n.Left)
		case storage.PredicateIn:
			if uint64(n.ArgumentCount)+uint64(len(stack))+uint64(len(seen)) > uint64(limit) {
				return nil, errPredicateBudget
			}
			stack = append(stack, n.Left)
			for k := uint32(0); k < n.ArgumentCount; k++ {
				stack = append(stack, p.Argument(n.FirstArgument+k))
			}
		}
	}
	out := make([]uint32, 0, len(seen))
	for idx := range seen {
		out = append(out, idx)
	}
	slices.Sort(out)
	return out, nil
}

// predCheckComparison reports whether a comparison node carries exact pinned
// semantics. Type inconsistencies are invalid; unknown operators, type pairs
// or collations are unsupported.
func predCheckComparison(p storage.Predicate, n storage.PredicateNode) (bool, error) {
	left, right := p.Node(n.Left), p.Node(n.Right)
	if left.Kind != storage.PredicateColumn && left.Kind != storage.PredicateConstant {
		return false, nil
	}
	if right.Kind != storage.PredicateColumn && right.Kind != storage.PredicateConstant {
		return false, nil
	}
	sig, ok := pinnedPredOps[n.Operator]
	if !ok {
		return false, nil
	}
	if sig.family == predText {
		if !predTextType(left.Type.ID) || !predTextType(right.Type.ID) {
			return false, nil
		}
	} else if left.Type.ID != sig.left || right.Type.ID != sig.right {
		return false, nil
	}
	if err := predCheckOperandStorage(left, sig.family); err != nil {
		return false, err
	}
	if err := predCheckOperandStorage(right, sig.family); err != nil {
		return false, err
	}
	if sig.family == predText {
		if !predTextCollation(n.Collation) {
			return false, nil
		}
		if !predTextCollation(left.Type.Collation) || !predTextCollation(right.Type.Collation) {
			return false, nil
		}
		return true, nil
	}
	if n.Collation != predCollNone || left.Type.Collation != predCollNone || right.Type.Collation != predCollNone {
		return false, nil
	}
	return true, nil
}

// predCheckIn reports whether an IN node uses a pinned equality with
// uniformly typed members. Anything else stays residual.
func predCheckIn(p storage.Predicate, n storage.PredicateNode) (bool, error) {
	probe := p.Node(n.Left)
	if probe.Kind != storage.PredicateColumn && probe.Kind != storage.PredicateConstant {
		return false, nil
	}
	sig, ok := pinnedPredOps[n.Operator]
	if !ok || sig.op != predEq {
		return false, nil
	}
	if sig.family == predText {
		if !predTextType(probe.Type.ID) {
			return false, nil
		}
	} else if probe.Type.ID != sig.left {
		return false, nil
	}
	if err := predCheckOperandStorage(probe, sig.family); err != nil {
		return false, err
	}
	for k := uint32(0); k < n.ArgumentCount; k++ {
		member := p.Node(p.Argument(n.FirstArgument + k))
		if member.Kind != storage.PredicateColumn && member.Kind != storage.PredicateConstant {
			return false, nil
		}
		if sig.family == predText {
			if !predTextType(member.Type.ID) {
				return false, nil
			}
		} else if member.Type.ID != sig.right {
			return false, nil
		}
		if err := predCheckOperandStorage(member, sig.family); err != nil {
			return false, err
		}
		if sig.family == predText && !predTextCollation(member.Type.Collation) {
			return false, nil
		}
	}
	if sig.family == predText {
		if !predTextCollation(n.Collation) || !predTextCollation(probe.Type.Collation) {
			return false, nil
		}
		return true, nil
	}
	if n.Collation != predCollNone || probe.Type.Collation != predCollNone {
		return false, nil
	}
	for k := uint32(0); k < n.ArgumentCount; k++ {
		if p.Node(p.Argument(n.FirstArgument+k)).Type.Collation != predCollNone {
			return false, nil
		}
	}
	return true, nil
}

func predCheckOperandStorage(n storage.PredicateNode, family predFamily) error {
	if n.Type.Storage != predFamilyStorage(family) {
		return predInvalid("operand type %+v inconsistent with operator family", n.Type)
	}
	return nil
}

func predUpgradeOperand(n storage.PredicateNode, colPos map[model.ColumnID]int32, kinds []predColKind) {
	if n.Kind != storage.PredicateColumn {
		return
	}
	pos := colPos[n.ColumnID]
	switch n.Type.Storage {
	case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean:
		kinds[pos] = predColInt
	case model.TypeFloat64:
		kinds[pos] = predColFloat
	case model.TypeString:
		kinds[pos] = predColText
	}
}

func predInternConst(prog *predicateProgram, c model.Value) int32 {
	idx := int32(len(prog.consts))
	prog.consts = append(prog.consts, c)
	return idx
}

func predScalarOperand(n storage.PredicateNode, p storage.Predicate, colPos map[model.ColumnID]int32, prog *predicateProgram) predScalarRef {
	if n.Kind == storage.PredicateColumn {
		return predScalarRef{col: colPos[n.ColumnID], cnst: -1}
	}
	return predScalarRef{col: -1, cnst: predInternConst(prog, p.Constant(n.ConstantIndex))}
}

// Filter evaluates the program over batch, intersecting the existing
// Selection. Input columns must arrive exactly in Columns() order. The output
// preserves input lane order and duplicates; when no rows qualify the output
// is a non-nil empty Selection. Selection and scratch buffers are borrowed
// until the next Filter or Close.
func (p *predicateProgram) Filter(ctx context.Context, batch *columnar.Batch, scratch *predicateScratch, maxBytes uint64) error {
	if batch == nil {
		return fmt.Errorf("%w: nil batch", storage.ErrInvalidScanPlan)
	}
	if scratch == nil || scratch.closed {
		return errPredicateScratchClosed
	}
	clear(scratch.xdata[:cap(scratch.xdata)])
	defer func() { clear(scratch.xdata[:cap(scratch.xdata)]) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.trivialTrue {
		if batch.Rows() == 0 && batch.Selection == nil {
			scratch.ensureEmpty()
			batch.Selection = scratch.empty
		}
		return nil
	}
	if len(batch.Columns) != len(p.cols) {
		return fmt.Errorf("%w: %d columns, program needs %d", storage.ErrInvalidScanPlan, len(batch.Columns), len(p.cols))
	}
	for i := range batch.Columns {
		if batch.Columns[i].Type != p.colTypes[i] {
			return fmt.Errorf("%w: column %d type %+v, program needs %+v", storage.ErrInvalidScanPlan, uint32(p.cols[i]), batch.Columns[i].Type, p.colTypes[i])
		}
	}
	if batch.Length == 0 {
		if len(batch.Selection) != 0 {
			return fmt.Errorf("%w: selection longer than empty batch", storage.ErrInvalidScanPlan)
		}
		scratch.ensureEmpty()
		batch.Selection = scratch.empty
		return nil
	}
	if batch.Length < 0 {
		return fmt.Errorf("%w: negative batch length", storage.ErrInvalidScanPlan)
	}
	for _, lane := range batch.Selection {
		if uint64(lane) >= uint64(batch.Length) {
			return fmt.Errorf("%w: selection lane out of bounds", storage.ErrInvalidScanPlan)
		}
	}
	for i := range batch.Columns {
		if batch.Columns[i].Length < batch.Length {
			return fmt.Errorf("%w: column %d short for batch length", storage.ErrInvalidScanPlan, uint32(p.cols[i]))
		}
		if err := batch.Columns[i].Validate(); err != nil {
			return fmt.Errorf("%w: %w", storage.ErrInvalidScanPlan, err)
		}
	}
	var incoming []uint32
	n := batch.Length
	if batch.Selection != nil {
		incoming = batch.Selection
		n = len(incoming)
	}
	if n == 0 {
		scratch.ensureEmpty()
		batch.Selection = scratch.empty
		return nil
	}
	base := uint64(256) + uint64(len(p.nodes))*128 + uint64(len(p.cols))*256
	perLane := uint64(4) + uint64(p.boolCount)*2
	for _, kind := range p.colKinds {
		perLane += predColBytes(kind)
	}
	retained := scratch.allocatedBytes()
	if retained > maxBytes || base > maxBytes-retained {
		return errPredicateBudget
	}
	if uint64(n) > (maxBytes-retained-base)/perLane {
		return errPredicateBudget
	}
	if err := scratch.prepare(n, len(p.cols), p.colKinds); err != nil {
		return err
	}
	if err := predExtractColumns(ctx, p, batch, scratch, incoming, n); err != nil {
		return err
	}
	tm, _, err := predEvalBools(ctx, p, scratch, n)
	if err != nil {
		return err
	}
	out := scratch.sel[:0]
	if incoming != nil {
		for i := range n {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if tm[i] == 1 {
				out = append(out, incoming[i])
			}
		}
	} else {
		for i := range n {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if tm[i] == 1 {
				out = append(out, uint32(i))
			}
		}
	}
	scratch.releaseMasks()
	batch.Selection = out
	return nil
}

func predColBytes(kind predColKind) uint64 {
	switch kind {
	case predColInt, predColFloat, predColText:
		return 9
	default:
		return 1
	}
}

func (s *predicateScratch) ensureEmpty() {
	if s.empty == nil {
		s.empty = []uint32{}
	}
	s.empty = s.empty[:0]
}

// prepare sizes lane-indexed extraction storage for n positions.
func (s *predicateScratch) prepare(n, ncols int, kinds []predColKind) error {
	if n <= 0 || ncols < 0 {
		return fmt.Errorf("%w: invalid lane layout", storage.ErrInvalidScanPlan)
	}
	if cap(s.sel) < n {
		s.sel = make([]uint32, 0, n)
	} else {
		s.sel = s.sel[:0]
	}
	s.xnull = resizePredSlices(s.xnull, ncols, n)
	s.xvalid = resizePredBools(s.xvalid, ncols)
	for i := range s.xvalid {
		s.xvalid[i] = false
	}
	s.xint = resizePredInt(s.xint, ncols, n, kinds, predColInt)
	s.xfloat = resizePredFloat(s.xfloat, ncols, n, kinds, predColFloat)
	s.xoff = resizePredU32(s.xoff, ncols, n, kinds, predColText)
	s.xlen = resizePredU32(s.xlen, ncols, n, kinds, predColText)
	if cap(s.xdata) < ncols {
		s.xdata = make([][]byte, ncols)
	} else {
		s.xdata = s.xdata[:ncols]
	}
	clear(s.xdata)
	return nil
}

func resizePredSlices(s [][]byte, ncols, n int) [][]byte {
	if cap(s) < ncols {
		s = make([][]byte, ncols)
	} else {
		s = s[:ncols]
	}
	for i := range s {
		if cap(s[i]) < n {
			s[i] = make([]byte, n)
		} else {
			s[i] = s[i][:n]
		}
	}
	return s
}

func resizePredBools(s []bool, n int) []bool {
	if cap(s) < n {
		s = make([]bool, n)
	} else {
		s = s[:n]
	}
	return s
}

func resizePredInt(s [][]int64, ncols, n int, kinds []predColKind, want predColKind) [][]int64 {
	if cap(s) < ncols {
		s = make([][]int64, ncols)
	} else {
		s = s[:ncols]
	}
	for i := range s {
		if kinds[i] != want {
			continue
		}
		if cap(s[i]) < n {
			s[i] = make([]int64, n)
		} else {
			s[i] = s[i][:n]
		}
	}
	return s
}

func resizePredFloat(s [][]float64, ncols, n int, kinds []predColKind, want predColKind) [][]float64 {
	if cap(s) < ncols {
		s = make([][]float64, ncols)
	} else {
		s = s[:ncols]
	}
	for i := range s {
		if kinds[i] != want {
			continue
		}
		if cap(s[i]) < n {
			s[i] = make([]float64, n)
		} else {
			s[i] = s[i][:n]
		}
	}
	return s
}

func resizePredU32(s [][]uint32, ncols, n int, kinds []predColKind, want predColKind) [][]uint32 {
	if cap(s) < ncols {
		s = make([][]uint32, ncols)
	} else {
		s = s[:ncols]
	}
	for i := range s {
		if kinds[i] != want {
			continue
		}
		if cap(s[i]) < n {
			s[i] = make([]uint32, n)
		} else {
			s[i] = s[i][:n]
		}
	}
	return s
}

func (s *predicateScratch) checkoutMask(n int) []byte {
	for i, buf := range s.pool {
		if cap(buf) >= n {
			copy(s.pool[i:], s.pool[i+1:])
			s.pool[len(s.pool)-1] = nil
			s.pool = s.pool[:len(s.pool)-1]
			return buf[:n]
		}
	}
	return make([]byte, n)
}

func (s *predicateScratch) releaseMasks() {
	// Checked-in masks stay pooled for the next Filter; extraction storage
	// stays valid until the next Filter or Close. The pool is bounded because
	// each evaluation checks in at most two masks per boolean node.
	for len(s.pool) > 64 {
		s.pool[len(s.pool)-1] = nil
		s.pool = s.pool[:len(s.pool)-1]
	}
}

func (s *predicateScratch) allocatedBytes() uint64 {
	return uint64(cap(s.sel)+cap(s.empty))*4 + uint64(cap(s.xvalid)) + uint64(cap(s.xdata))*24 +
		predBufferBytes(s.xnull) + predBufferBytes(s.xint) + predBufferBytes(s.xfloat) +
		predBufferBytes(s.xoff) + predBufferBytes(s.xlen) + predBufferBytes(s.pool)
}

func predBufferBytes[T any](buffers [][]T) uint64 {
	var value T
	total := uint64(cap(buffers)) * uint64(unsafe.Sizeof([]T{}))
	for _, buffer := range buffers[:cap(buffers)] {
		total += uint64(cap(buffer)) * uint64(unsafe.Sizeof(value))
	}
	return total
}

// predExtractColumns materializes lane-indexed values for every required
// column. Encoding dispatch happens once per column; inner lane loops contain
// no interface dispatch. NULL dictionary lanes never read dictionary codes.
func predExtractColumns(ctx context.Context, p *predicateProgram, batch *columnar.Batch, s *predicateScratch, incoming []uint32, n int) error {
	for c := range p.cols {
		if err := ctx.Err(); err != nil {
			return err
		}
		vec := &batch.Columns[c]
		switch p.colKinds[c] {
		case predNullOnly:
			predExtractNulls(vec, s.xnull[c], incoming, n)
		case predColInt:
			predExtractInts(vec, s.xnull[c], s.xint[c], incoming, n)
		case predColFloat:
			predExtractFloats(vec, s.xnull[c], s.xfloat[c], incoming, n)
		case predColText:
			s.xdata[c] = vec.Data
			predExtractTexts(vec, s.xnull[c], s.xoff[c], s.xlen[c], incoming, n)
		}
		s.xvalid[c] = true
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

func predLaneNull(nulls []byte, lane int) bool {
	return len(nulls) != 0 && nulls[lane/8]&(1<<uint(lane%8)) != 0
}

func predExtractNulls(vec *columnar.Vector, out []byte, incoming []uint32, n int) {
	if vec.Encoding == columnar.Constant {
		null := predLaneNull(vec.Nulls, 0)
		for i := range n {
			if null {
				out[i] = 1
			} else {
				out[i] = 0
			}
		}
		return
	}
	if incoming != nil {
		for i := range n {
			lane := int(incoming[i])
			if predLaneNull(vec.Nulls, vec.Offset+lane) {
				out[i] = 1
			} else {
				out[i] = 0
			}
		}
		return
	}
	for i := range n {
		if predLaneNull(vec.Nulls, vec.Offset+i) {
			out[i] = 1
		} else {
			out[i] = 0
		}
	}
}

func predExtractInts(vec *columnar.Vector, nulls []byte, vals []int64, incoming []uint32, n int) {
	if vec.Encoding == columnar.Constant {
		null := predLaneNull(vec.Nulls, 0)
		var v int64
		if !null && len(vec.Int64) > 0 {
			v = vec.Int64[0]
		}
		for i := range n {
			if null {
				nulls[i] = 1
			} else {
				nulls[i] = 0
				vals[i] = v
			}
		}
		return
	}
	if vec.Encoding == columnar.Dictionary {
		if incoming != nil {
			for i := range n {
				lane := vec.Offset + int(incoming[i])
				if predLaneNull(vec.Nulls, lane) {
					nulls[i] = 1
					continue
				}
				nulls[i] = 0
				vals[i] = vec.Int64[vec.Indices[lane]]
			}
			return
		}
		for i := range n {
			lane := vec.Offset + i
			if predLaneNull(vec.Nulls, lane) {
				nulls[i] = 1
				continue
			}
			nulls[i] = 0
			vals[i] = vec.Int64[vec.Indices[lane]]
		}
		return
	}
	if incoming != nil {
		for i := range n {
			lane := vec.Offset + int(incoming[i])
			if predLaneNull(vec.Nulls, lane) {
				nulls[i] = 1
				continue
			}
			nulls[i] = 0
			vals[i] = vec.Int64[lane]
		}
		return
	}
	for i := range n {
		lane := vec.Offset + i
		if predLaneNull(vec.Nulls, lane) {
			nulls[i] = 1
			continue
		}
		nulls[i] = 0
		vals[i] = vec.Int64[lane]
	}
}

func predExtractFloats(vec *columnar.Vector, nulls []byte, vals []float64, incoming []uint32, n int) {
	if vec.Encoding == columnar.Constant {
		null := predLaneNull(vec.Nulls, 0)
		var v float64
		if !null && len(vec.Float64) > 0 {
			v = vec.Float64[0]
		}
		for i := range n {
			if null {
				nulls[i] = 1
			} else {
				nulls[i] = 0
				vals[i] = v
			}
		}
		return
	}
	if vec.Encoding == columnar.Dictionary {
		if incoming != nil {
			for i := range n {
				lane := vec.Offset + int(incoming[i])
				if predLaneNull(vec.Nulls, lane) {
					nulls[i] = 1
					continue
				}
				nulls[i] = 0
				vals[i] = vec.Float64[vec.Indices[lane]]
			}
			return
		}
		for i := range n {
			lane := vec.Offset + i
			if predLaneNull(vec.Nulls, lane) {
				nulls[i] = 1
				continue
			}
			nulls[i] = 0
			vals[i] = vec.Float64[vec.Indices[lane]]
		}
		return
	}
	if incoming != nil {
		for i := range n {
			lane := vec.Offset + int(incoming[i])
			if predLaneNull(vec.Nulls, lane) {
				nulls[i] = 1
				continue
			}
			nulls[i] = 0
			vals[i] = vec.Float64[lane]
		}
		return
	}
	for i := range n {
		lane := vec.Offset + i
		if predLaneNull(vec.Nulls, lane) {
			nulls[i] = 1
			continue
		}
		nulls[i] = 0
		vals[i] = vec.Float64[lane]
	}
}

func predExtractTexts(vec *columnar.Vector, nulls []byte, offs, lens []uint32, incoming []uint32, n int) {
	if vec.Encoding == columnar.Constant {
		null := predLaneNull(vec.Nulls, 0)
		var off, length uint32
		if !null && len(vec.Ranges) > 0 {
			off, length = vec.Ranges[0].Offset, vec.Ranges[0].Length
		}
		for i := range n {
			if null {
				nulls[i] = 1
			} else {
				nulls[i] = 0
				offs[i], lens[i] = off, length
			}
		}
		return
	}
	if vec.Encoding == columnar.Dictionary {
		if incoming != nil {
			for i := range n {
				lane := vec.Offset + int(incoming[i])
				if predLaneNull(vec.Nulls, lane) {
					nulls[i] = 1
					continue
				}
				nulls[i] = 0
				r := vec.Ranges[vec.Indices[lane]]
				offs[i], lens[i] = r.Offset, r.Length
			}
			return
		}
		for i := range n {
			lane := vec.Offset + i
			if predLaneNull(vec.Nulls, lane) {
				nulls[i] = 1
				continue
			}
			nulls[i] = 0
			r := vec.Ranges[vec.Indices[lane]]
			offs[i], lens[i] = r.Offset, r.Length
		}
		return
	}
	if incoming != nil {
		for i := range n {
			lane := vec.Offset + int(incoming[i])
			if predLaneNull(vec.Nulls, lane) {
				nulls[i] = 1
				continue
			}
			nulls[i] = 0
			r := vec.Ranges[lane]
			offs[i], lens[i] = r.Offset, r.Length
		}
		return
	}
	for i := range n {
		lane := vec.Offset + i
		if predLaneNull(vec.Nulls, lane) {
			nulls[i] = 1
			continue
		}
		nulls[i] = 0
		r := vec.Ranges[lane]
		offs[i], lens[i] = r.Offset, r.Length
	}
}

// predEvalBools evaluates every boolean node in topological order with
// reference-counted mask reuse, returning the root TRUE and NULL masks.
// Masks are position-indexed over the incoming lanes.
func predEvalBools(ctx context.Context, p *predicateProgram, s *predicateScratch, n int) ([]byte, []byte, error) {
	users := make([]int, len(p.nodes))
	for _, cn := range p.nodes {
		switch cn.kind {
		case predAnd, predOr:
			users[cn.a]++
			users[cn.b]++
		case predNullTest:
			if cn.boolChild {
				users[cn.a]++
			}
		}
	}
	users[p.root]++
	tm := make([][]byte, len(p.nodes))
	nm := make([][]byte, len(p.nodes))
	release := func(idx uint32) {
		users[idx]--
		if users[idx] == 0 {
			if tm[idx] != nil {
				s.pool = append(s.pool, tm[idx])
				tm[idx] = nil
			}
			if nm[idx] != nil {
				s.pool = append(s.pool, nm[idx])
				nm[idx] = nil
			}
		}
	}
	for idx := range p.nodes {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		cn := &p.nodes[idx]
		switch cn.kind {
		case predScalarColumn, predScalarConst:
			continue
		}
		tm[idx] = s.checkoutMask(n)
		nm[idx] = s.checkoutMask(n)
		var err error
		switch cn.kind {
		case predBoolColumn:
			predEvalBoolColumn(cn, s, tm[idx], nm[idx], n)
		case predBoolConst:
			predEvalBoolConst(cn, tm[idx], nm[idx], n)
		case predCompare:
			err = predEvalCompare(ctx, p, cn, s, tm[idx], nm[idx], n)
		case predNullTest:
			err = predEvalNullTest(ctx, p, cn, s, tm[idx], nm[idx], tm, nm, n)
		case predIn:
			err = predEvalIn(ctx, p, cn, s, tm[idx], nm[idx], n)
		case predAnd:
			err = predEvalAnd(ctx, tm[cn.a], nm[cn.a], tm[cn.b], nm[cn.b], tm[idx], nm[idx], n)
		case predOr:
			err = predEvalOr(ctx, tm[cn.a], nm[cn.a], tm[cn.b], nm[cn.b], tm[idx], nm[idx], n)
		}
		if err != nil {
			return nil, nil, err
		}
		switch cn.kind {
		case predAnd, predOr:
			release(cn.a)
			release(cn.b)
		case predNullTest:
			if cn.boolChild {
				release(cn.a)
			}
		}
	}
	return tm[p.root], nm[p.root], nil
}

func predEvalBoolColumn(cn *predCompiledNode, s *predicateScratch, t, n []byte, lanes int) {
	nulls := s.xnull[cn.col]
	vals := s.xint[cn.col]
	for i := range lanes {
		if nulls[i] == 1 {
			t[i], n[i] = 0, 1
		} else {
			n[i] = 0
			if vals[i] == 1 {
				t[i] = 1
			} else {
				t[i] = 0
			}
		}
	}
}

func predEvalBoolConst(cn *predCompiledNode, t, n []byte, lanes int) {
	for i := range lanes {
		if cn.bnull {
			t[i], n[i] = 0, 1
		} else {
			n[i] = 0
			if cn.bval {
				t[i] = 1
			} else {
				t[i] = 0
			}
		}
	}
}

func predEvalCompare(ctx context.Context, p *predicateProgram, cn *predCompiledNode, s *predicateScratch, t, n []byte, lanes int) error {
	switch cn.family {
	case predInt, predBool, predTimestamp:
		return predEvalIntCompare(ctx, p, cn, s, t, n, lanes)
	case predFloat:
		return predEvalFloatCompare(ctx, p, cn, s, t, n, lanes)
	case predText:
		return predEvalTextCompare(ctx, p, cn, s, t, n, lanes)
	}
	return fmt.Errorf("%w: unknown comparison family", storage.ErrInvalidScanPlan)
}

func predApplyOp(c int, op predCmpOp) bool {
	switch op {
	case predEq:
		return c == 0
	case predNe:
		return c != 0
	case predLt:
		return c < 0
	case predGt:
		return c > 0
	case predLe:
		return c <= 0
	case predGe:
		return c >= 0
	}
	return false
}

func predIntSide(p *predicateProgram, s *predicateScratch, ref predScalarRef, i int) (bool, int64) {
	if ref.col >= 0 {
		if s.xnull[ref.col][i] == 1 {
			return true, 0
		}
		return false, s.xint[ref.col][i]
	}
	c := p.consts[ref.cnst]
	if c.IsNull {
		return true, 0
	}
	return false, c.IntVal
}

func predEvalIntCompare(ctx context.Context, p *predicateProgram, cn *predCompiledNode, s *predicateScratch, t, n []byte, lanes int) error {
	for i := range lanes {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		lNull, lVal := predIntSide(p, s, cn.left, i)
		rNull, rVal := predIntSide(p, s, cn.right, i)
		if lNull || rNull {
			t[i], n[i] = 0, 1
			continue
		}
		c := 0
		if lVal < rVal {
			c = -1
		} else if lVal > rVal {
			c = 1
		}
		n[i] = 0
		if predApplyOp(c, cn.op) {
			t[i] = 1
		} else {
			t[i] = 0
		}
	}
	return nil
}

func predFloatSide(p *predicateProgram, s *predicateScratch, ref predScalarRef, i int) (bool, float64) {
	if ref.col >= 0 {
		if s.xnull[ref.col][i] == 1 {
			return true, 0
		}
		return false, s.xfloat[ref.col][i]
	}
	c := p.consts[ref.cnst]
	if c.IsNull {
		return true, 0
	}
	return false, c.FloatVal
}

// predFloatOrder implements PostgreSQL NaN ordering: NaN exceeds every
// non-NaN value and equals only NaN.
func predFloatOrder(a, b float64) int {
	aNaN, bNaN := math.IsNaN(a), math.IsNaN(b)
	if aNaN && bNaN {
		return 0
	}
	if aNaN {
		return 1
	}
	if bNaN {
		return -1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func predEvalFloatCompare(ctx context.Context, p *predicateProgram, cn *predCompiledNode, s *predicateScratch, t, n []byte, lanes int) error {
	for i := range lanes {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		lNull, lVal := predFloatSide(p, s, cn.left, i)
		rNull, rVal := predFloatSide(p, s, cn.right, i)
		if lNull || rNull {
			t[i], n[i] = 0, 1
			continue
		}
		n[i] = 0
		if predApplyOp(predFloatOrder(lVal, rVal), cn.op) {
			t[i] = 1
		} else {
			t[i] = 0
		}
	}
	return nil
}

func predTextSide(p *predicateProgram, s *predicateScratch, ref predScalarRef, i int) (bool, []byte, string) {
	if ref.col >= 0 {
		if s.xnull[ref.col][i] == 1 {
			return true, nil, ""
		}
		off, length := uint64(s.xoff[ref.col][i]), uint64(s.xlen[ref.col][i])
		base := s.xdata[ref.col]
		if off+length > uint64(len(base)) {
			return true, nil, ""
		}
		return false, base[off : off+length], ""
	}
	c := p.consts[ref.cnst]
	if c.IsNull {
		return true, nil, ""
	}
	return false, nil, c.StrVal
}

func predCompareBytesStr(b []byte, str string) int {
	n := min(len(b), len(str))
	for i := range n {
		if b[i] < str[i] {
			return -1
		}
		if b[i] > str[i] {
			return 1
		}
	}
	switch {
	case len(b) < len(str):
		return -1
	case len(b) > len(str):
		return 1
	}
	return 0
}

func predEvalTextCompare(ctx context.Context, p *predicateProgram, cn *predCompiledNode, s *predicateScratch, t, n []byte, lanes int) error {
	for i := range lanes {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		lNull, lBytes, lStr := predTextSide(p, s, cn.left, i)
		rNull, rBytes, rStr := predTextSide(p, s, cn.right, i)
		if lNull || rNull {
			t[i], n[i] = 0, 1
			continue
		}
		var c int
		switch {
		case cn.left.col >= 0 && cn.right.col >= 0:
			c = bytes.Compare(lBytes, rBytes)
		case cn.left.col >= 0:
			c = predCompareBytesStr(lBytes, rStr)
		case cn.right.col >= 0:
			c = -predCompareBytesStr(rBytes, lStr)
		default:
			c = strings.Compare(lStr, rStr)
		}
		n[i] = 0
		if predApplyOp(c, cn.op) {
			t[i] = 1
		} else {
			t[i] = 0
		}
	}
	return nil
}

func predEvalNullTest(ctx context.Context, p *predicateProgram, cn *predCompiledNode, s *predicateScratch, t, n []byte, tm, nm [][]byte, lanes int) error {
	if cn.boolChild {
		cnm := nm[cn.a]
		for i := range lanes {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			n[i] = 0
			isNull := cnm[i] == 1
			if cn.negated {
				isNull = !isNull
			}
			if isNull {
				t[i] = 1
			} else {
				t[i] = 0
			}
		}
		return nil
	}
	for i := range lanes {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		var isNull bool
		if cn.left.col >= 0 {
			isNull = s.xnull[cn.left.col][i] == 1
		} else {
			isNull = p.consts[cn.left.cnst].IsNull
		}
		n[i] = 0
		if cn.negated {
			isNull = !isNull
		}
		if isNull {
			t[i] = 1
		} else {
			t[i] = 0
		}
	}
	return nil
}

// predScalarEqual reports PostgreSQL equality between one probe lane and one
// member lane. Nulls never compare; the caller tracks them for three-valued
// IN logic.
func predScalarEqual(p *predicateProgram, s *predicateScratch, family predFamily, probe, member predScalarRef, i int) (probeNull, memberNull, equal bool) {
	switch family {
	case predInt, predBool, predTimestamp:
		pn, pv := predIntSide(p, s, probe, i)
		mn, mv := predIntSide(p, s, member, i)
		return pn, mn, !pn && !mn && pv == mv
	case predFloat:
		pn, pv := predFloatSide(p, s, probe, i)
		mn, mv := predFloatSide(p, s, member, i)
		return pn, mn, !pn && !mn && predFloatOrder(pv, mv) == 0
	case predText:
		pn, pb, ps := predTextSide(p, s, probe, i)
		mn, mb, ms := predTextSide(p, s, member, i)
		if pn || mn {
			return pn, mn, false
		}
		var c int
		switch {
		case probe.col >= 0 && member.col >= 0:
			c = bytes.Compare(pb, mb)
		case probe.col >= 0:
			c = predCompareBytesStr(pb, ms)
		case member.col >= 0:
			c = -predCompareBytesStr(mb, ps)
		default:
			if ps == ms {
				c = 0
			} else if ps < ms {
				c = -1
			} else {
				c = 1
			}
		}
		return false, false, c == 0
	}
	return true, true, false
}

func predEvalIn(ctx context.Context, p *predicateProgram, cn *predCompiledNode, s *predicateScratch, t, n []byte, lanes int) error {
	family := p.inFamily(cn)
	for i := range lanes {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		// Probe nullability without consuming a member comparison.
		probeNull := predProbeNull(p, s, family, cn.left, i)
		if len(cn.members) == 0 {
			// An empty list matches nothing, even for a NULL probe.
			t[i], n[i] = 0, 0
		} else if probeNull {
			t[i], n[i] = 0, 1
		} else {
			matched, hasNull := predFindMember(p, cn, s, family, i)
			switch {
			case matched:
				t[i], n[i] = 1, 0
			case hasNull:
				t[i], n[i] = 0, 1
			default:
				t[i], n[i] = 0, 0
			}
		}
		if cn.negated && n[i] == 0 {
			if t[i] == 1 {
				t[i] = 0
			} else {
				t[i] = 1
			}
		}
	}
	return nil
}

func predFindMember(p *predicateProgram, cn *predCompiledNode, s *predicateScratch, family predFamily, lane int) (bool, bool) {
	hasNull := false
	for _, member := range cn.members {
		_, null, equal := predScalarEqual(p, s, family, cn.left, member, lane)
		if equal {
			return true, hasNull
		}
		hasNull = hasNull || null
	}
	return false, hasNull
}

// inFamily recovers the operand family from the compiled probe reference.
func (p *predicateProgram) inFamily(cn *predCompiledNode) predFamily {
	ref := cn.left
	if ref.col >= 0 {
		switch p.colKinds[ref.col] {
		case predColFloat:
			return predFloat
		case predColText:
			return predText
		}
		st := p.colTypes[ref.col].Storage
		if st == model.TypeTimestamp {
			return predTimestamp
		}
		if st == model.TypeBoolean {
			return predBool
		}
		return predInt
	}
	switch p.consts[ref.cnst].Type {
	case model.TypeFloat64:
		return predFloat
	case model.TypeString:
		return predText
	case model.TypeTimestamp:
		return predTimestamp
	case model.TypeBoolean:
		return predBool
	}
	return predInt
}

func predProbeNull(p *predicateProgram, s *predicateScratch, family predFamily, ref predScalarRef, i int) bool {
	switch family {
	case predFloat:
		null, _ := predFloatSide(p, s, ref, i)
		return null
	case predText:
		null, _, _ := predTextSide(p, s, ref, i)
		return null
	default:
		null, _ := predIntSide(p, s, ref, i)
		return null
	}
}

func predEvalAnd(ctx context.Context, at, an, bt, bn, t, n []byte, lanes int) error {
	for i := range lanes {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		aTrue, aNull := at[i] == 1, an[i] == 1
		bTrue, bNull := bt[i] == 1, bn[i] == 1
		aFalse, bFalse := !aTrue && !aNull, !bTrue && !bNull
		switch {
		case aTrue && bTrue:
			t[i], n[i] = 1, 0
		case aFalse || bFalse:
			t[i], n[i] = 0, 0
		default:
			t[i], n[i] = 0, 1
		}
	}
	return nil
}

func predEvalOr(ctx context.Context, at, an, bt, bn, t, n []byte, lanes int) error {
	for i := range lanes {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		aTrue, aNull := at[i] == 1, an[i] == 1
		bTrue, bNull := bt[i] == 1, bn[i] == 1
		aFalse, bFalse := !aTrue && !aNull, !bTrue && !bNull
		switch {
		case aTrue || bTrue:
			t[i], n[i] = 1, 0
		case aFalse && bFalse:
			t[i], n[i] = 0, 0
		default:
			t[i], n[i] = 0, 1
		}
	}
	return nil
}
