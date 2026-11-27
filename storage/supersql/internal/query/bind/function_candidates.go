package bind

import (
	"iter"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func (b *binder) signaturesNamed(name string) iter.Seq[builtin.Signature] {
	return func(yield func(builtin.Signature) bool) {
		for signature := range builtin.SignaturesNamed(name) {
			if !yield(signature) {
				return
			}
		}
		for signature := range b.options.CatalogRelations.InformationSchemaFunctionsNamed(name) {
			if !yield(signature) {
				return
			}
		}
	}
}

func (b *binder) lookupSignature(id model.FunctionID) (builtin.Signature, bool) {
	if function, ok := b.options.CatalogRelations.ResolveInformationSchemaFunction(id); ok {
		return function.Signature, true
	}
	return builtin.LookupSignature(id)
}

func (b *binder) builtinCandidate(name string, call callArguments, pos int, variadicArray bool) (builtin.Signature, bool, error) {
	args := call.values
	if len(args) > builtin.MaxCallArguments {
		return builtin.Signature{}, false, bindError("54023", "cannot pass more than 100 arguments to a function", pos)
	}
	var selected builtin.Signature
	count := 0
	for signature := range b.signaturesNamed(name) {
		if err := b.ctx.Err(); err != nil {
			return selected, false, err
		}
		var matches bool
		signature, matches = call.projectBuiltin(signature, variadicArray)
		if !matches {
			continue
		}
		if b.functionCandidateScore(signature, args) >= 0 {
			count++
			selected = signature
		}
	}
	if count <= 1 {
		return selected, count == 1, nil
	}
	if uint64(count) > b.options.Limits.MaxBytes/uint64(unsafe.Sizeof(builtin.Signature{})) {
		return selected, true, bindError("54000", "builtin candidate metadata limit exceeded", pos)
	}
	candidates := make([]builtin.Signature, 0, count)
	bestExact, bestPreferred := -1, -1
	for signature := range b.signaturesNamed(name) {
		if err := b.ctx.Err(); err != nil {
			return selected, false, err
		}
		var matches bool
		signature, matches = call.projectBuiltin(signature, variadicArray)
		if !matches {
			continue
		}
		if b.functionCandidateScore(signature, args) < 0 {
			continue
		}
		exact := 0
		for i, arg := range args {
			if source := b.argumentID(arg); source != 0 && source == signatureArgument(signature, i) {
				exact++
			}
		}
		preferred := b.preferredConversions(signature, args)
		better := exact > bestExact || exact == bestExact && preferred > bestPreferred
		if better {
			bestExact, bestPreferred = exact, preferred
			candidates = candidates[:0]
		}
		if exact == bestExact && preferred == bestPreferred {
			candidates = append(candidates, signature)
		}
	}
	var err error
	candidates, err = b.preferNonVariadicBuiltin(candidates, len(args))
	if err != nil {
		return selected, false, err
	}
	if len(candidates) == 1 {
		return candidates[0], true, nil
	}
	candidates = b.unknownBuiltinCandidates(candidates, args)
	if len(candidates) == 1 {
		return candidates[0], true, nil
	}
	if candidate, ok := b.uniformKnownBuiltin(candidates, args); ok {
		return candidate, true, nil
	}
	return selected, true, bindError("42725", "function call is ambiguous", pos)
}

func (b *binder) preferNonVariadicBuiltin(candidates []builtin.Signature, argumentCount int) ([]builtin.Signature, error) {
	kept := 0
	for _, candidate := range candidates {
		if err := b.ctx.Err(); err != nil {
			return nil, err
		}
		masked := false
		if candidate.Variadic != 0 && candidate.Variadic != 2276 {
			masked = hasMatchingFixedBuiltin(candidate, candidates, argumentCount)
		}
		if !masked {
			candidates[kept] = candidate
			kept++
		}
	}
	return candidates[:kept], nil
}

func hasMatchingFixedBuiltin(candidate builtin.Signature, candidates []builtin.Signature, count int) bool {
	for _, fixed := range candidates {
		if fixed.Variadic == 0 && sameBuiltinArguments(candidate, fixed, count) {
			return true
		}
	}
	return false
}

func sameBuiltinArguments(left, right builtin.Signature, count int) bool {
	for i := range count {
		if signatureArgument(left, i) != signatureArgument(right, i) {
			return false
		}
	}
	return true
}

func (b *binder) unknownBuiltinCandidates(candidates []builtin.Signature, args []expression) []builtin.Signature {
	type preference struct {
		category  byte
		preferred bool
	}
	var inline [8]preference
	slots := inline[:]
	if len(args) > len(inline) {
		slots = make([]preference, len(args))
	}
	slots = slots[:len(args)]
	unknown := false
	for i, arg := range args {
		if id := b.argumentID(arg); id != 0 && id != 705 {
			continue
		}
		unknown = true
		conflict := false
		for _, candidate := range candidates {
			typ, ok := builtin.LookupType(signatureArgument(candidate, i))
			if !ok {
				return candidates
			}
			switch {
			case slots[i].category == 0:
				slots[i] = preference{typ.Category, typ.Preferred}
			case slots[i].category == typ.Category:
				slots[i].preferred = slots[i].preferred || typ.Preferred
			case typ.Category == 'S':
				// Unknown literals prefer the string category when it is available.
				slots[i] = preference{typ.Category, typ.Preferred}
			default:
				conflict = true
			}
		}
		if conflict && slots[i].category != 'S' {
			return candidates
		}
	}
	if !unknown {
		return candidates
	}
	kept := 0
	for _, candidate := range candidates {
		matches := true
		for i, preference := range slots {
			if preference.category == 0 {
				continue
			}
			typ, _ := builtin.LookupType(signatureArgument(candidate, i))
			wrongPreference := preference.preferred && !typ.Preferred
			if typ.Category != preference.category || wrongPreference {
				matches = false
				break
			}
		}
		if matches {
			candidates[kept] = candidate
			kept++
		}
	}
	if kept == 0 {
		return candidates
	}
	return candidates[:kept]
}

func (b *binder) uniformKnownBuiltin(candidates []builtin.Signature, args []expression) (builtin.Signature, bool) {
	var known model.TypeID
	var example expression
	unknown := false
	for _, arg := range args {
		id := b.argumentID(arg)
		if id == 0 || id == 705 {
			unknown = true
			continue
		}
		if known != 0 && id != known {
			return builtin.Signature{}, false
		}
		known, example = id, arg
	}
	if known == 0 || !unknown {
		return builtin.Signature{}, false
	}
	layout := example.typ
	if layout.Type.ID == 0 {
		var ok bool
		layout, ok = b.params[example.parameter]
		if !ok {
			layout = b.primitive(known)
		}
	}
	var inline [8]expression
	assumed := inline[:]
	if len(args) > len(inline) {
		assumed = make([]expression, len(args))
	}
	assumed = assumed[:len(args)]
	for i, arg := range args {
		assumed[i] = arg
		if id := b.argumentID(arg); id == 0 || id == 705 {
			assumed[i] = expression{typ: layout}
		}
	}
	var selected builtin.Signature
	found := false
	for _, candidate := range candidates {
		if b.functionCandidateScore(candidate, assumed) < 0 {
			continue
		}
		if found {
			return builtin.Signature{}, false
		}
		selected, found = candidate, true
	}
	return selected, found
}
