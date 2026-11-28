package builtin

import (
	"iter"
	"sort"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// MaxCallArguments is PostgreSQL's bound on passed arguments, not the number
// of elements inside one explicit VARIADIC array.
const MaxCallArguments = 100

// Signature is catalog metadata, not evidence of an executable implementation.
// PostgreSQL 18.4's builtin signatures have at most eight input arguments.
// Fixed arrays keep the immutable catalog free of per-signature heap objects.
type Signature struct {
	ID            model.FunctionID
	Name          string
	Arguments     [8]model.TypeID
	ArgumentCount uint8
	Result        model.TypeID
	Variadic      model.TypeID
	Strict        bool
	ReturnsSet    bool
	Volatility    byte
	Kind          byte
	Language        model.OID
	Cost            float64
	Rows            float64
	Support         model.FunctionID
	SecurityDefiner bool
	Leakproof       bool
	Parallel        byte
	Source          string
	AggregateKind                  byte
	AggregateDirectArguments       uint16
	AggregateTransition            model.FunctionID
	AggregateFinal                 model.FunctionID
	AggregateCombine               model.FunctionID
	AggregateSerial                model.FunctionID
	AggregateDeserial              model.FunctionID
	AggregateMovingTransition      model.FunctionID
	AggregateMovingInverse         model.FunctionID
	AggregateMovingFinal           model.FunctionID
	AggregateFinalExtra            bool
	AggregateMovingFinalExtra      bool
	AggregateFinalModify           byte
	AggregateMovingFinalModify     byte
	AggregateSortOperator          model.OperatorID
	AggregateTransitionType        model.TypeID
	AggregateTransitionSpace       int32
	AggregateMovingTransitionType  model.TypeID
	AggregateMovingTransitionSpace int32
	AggregateInitialValue          string
	AggregateMovingInitialValue    string
	AggregateHasInitialValue       bool
	AggregateHasMovingInitialValue bool
}
type AggregateKind uint8

const (
	OrdinaryAggregate AggregateKind = iota + 1
	OrderedSetAggregate
	HypotheticalSetAggregate
)

type AggregateMetadata struct {
	Kind            AggregateKind
	DirectArguments uint8
}

func LookupAggregateMetadata(id model.FunctionID) (AggregateMetadata, bool) {
	switch id {
	case 3972, 3974, 3976, 3978, 3980, 3982:
		return AggregateMetadata{Kind: OrderedSetAggregate, DirectArguments: 1}, true
	case 3984:
		return AggregateMetadata{Kind: OrderedSetAggregate}, true
	case 3986, 3988, 3990, 3992:
		return AggregateMetadata{Kind: HypotheticalSetAggregate, DirectArguments: 1}, true
	}
	return AggregateMetadata{}, false
}

func LookupSignature(id model.FunctionID) (Signature, bool) {
	i := sort.Search(len(signatures), func(i int) bool { return signatures[i].ID >= id })
	if i == len(signatures) || signatures[i].ID != id {
		return Signature{}, false
	}
	return signatures[i], true
}

func Signatures() iter.Seq[Signature] {
	return func(yield func(Signature) bool) {
		for _, signature := range signatures {
			if !yield(signature) {
				return
			}
		}
	}
}

// SignaturesNamed returns values; callers cannot mutate the catalog's arrays.
// Names are already normalized by PostgreSQL identifier parsing.
func SignaturesNamed(name string) iter.Seq[Signature] {
	return func(yield func(Signature) bool) {
		start := sort.Search(len(signatureNames), func(i int) bool { return signatures[signatureNames[i]].Name >= name })
		for i := start; i < len(signatureNames); i++ {
			signature := signatures[signatureNames[i]]
			if signature.Name != name || !yield(signature) {
				return
			}
		}
	}
}
