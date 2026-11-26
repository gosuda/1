package aggregate

import (
	"context"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type jsonAggregatePlan struct {
	encoders   [2]*builtin.JSONValueEncoder
	object     bool
	binary     bool
	strict     bool
	unique     bool
	structured bool
}

type jsonAggregateEntry struct {
	key   string
	value string
	skip  bool
}

type jsonAggregateState struct {
	buffer     []byte
	entries    []jsonAggregateEntry
	entryBytes uint64
	duplicate  bool
	emitted    bool
	framing    int
}

func isJSONAggregate(id model.FunctionID) bool {
	switch id {
	case 3175, 3197, 3267, 3270, 6276, 6280, 6281, 6282, 6284, 6288, 6289, 6290:
		return true
	}
	return false
}

func compileJSONAggregate(ctx context.Context, call Call, layouts []model.TypeLayout, options builtin.ResolveOptions) (*jsonAggregatePlan, error) {
	p := &jsonAggregatePlan{object: len(call.Arguments) == 2}
	switch call.Function {
	case 3267, 3270, 6284, 6288, 6289, 6290:
		p.binary = true
	}
	switch call.Function {
	case 6276, 6280, 6282, 6284, 6288, 6290:
		p.strict = true
	}
	switch call.Function {
	case 6281, 6282, 6289, 6290:
		p.unique = true
	}
	remaining := uint64(2048)
	for i, column := range call.Arguments {
		encoder, err := builtin.ResolveJSONValue(ctx, layouts[column], options, remaining)
		if err != nil {
			return nil, err
		}
		p.encoders[i] = encoder
		remaining -= encoder.OwnedBytes()
	}
	typ := layouts[call.Arguments[0]].Type.Storage
	p.structured = typ == model.TypeArray || typ == model.TypeComposite
	return p, nil
}

func (c *cursor) jsonBudget() builtin.CallBudget {
	// Per-group state is admitted separately; this is the shared 32M scratch,
	// excluding overlapping token strings, assembly, and buffer growth.
	return builtin.CallBudget{OutputBytes: int64(c.options.MaxRecordBytes), MemoryBytes: c.options.MaxRecordBytes * 24}
}

func (c *cursor) appendJSON(ctx context.Context, state *jsonAggregateState, parts ...string) error {
	length := len(state.buffer)
	limit := int(c.options.MaxRecordBytes) - state.framing
	for _, part := range parts {
		if len(part) > limit-length {
			return resource("JSON aggregate result exceeds byte limit", model.ErrValueBudget)
		}
		length += len(part)
	}
	if length > cap(state.buffer) {
		next := make([]byte, len(state.buffer), min(limit, max(length, cap(state.buffer)*2)))
		for offset := 0; offset < len(state.buffer); offset += stringAggregateCopyBytes {
			if err := c.check(ctx); err != nil {
				return err
			}
			copy(next[offset:min(offset+stringAggregateCopyBytes, len(next))], state.buffer[offset:])
		}
		state.buffer = next
	}
	for _, part := range parts {
		for offset := 0; offset < len(part); offset += stringAggregateCopyBytes {
			if err := c.check(ctx); err != nil {
				return err
			}
			state.buffer = append(state.buffer, part[offset:min(offset+stringAggregateCopyBytes, len(part))]...)
		}
	}
	return nil
}

func (c *cursor) jsonEntry(ctx context.Context, state *jsonAggregateState, key string) (int, bool, error) {
	lo, hi := 0, len(state.entries)
	for lo < hi {
		if err := c.check(ctx); err != nil {
			return 0, false, err
		}
		mid := int(uint(lo+hi) >> 1)
		if state.entries[mid].key < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < len(state.entries) && state.entries[lo].key == key, nil
}

func (c *cursor) putJSONEntry(ctx context.Context, state *jsonAggregateState, index int, found bool, entry jsonAggregateEntry) error {
	payload := state.entryBytes + uint64(len(entry.key)+len(entry.value))
	if found {
		payload -= uint64(len(state.entries[index].key) + len(state.entries[index].value))
	}
	capacity := cap(state.entries)
	if !found && len(state.entries) == capacity {
		capacity = max(1, capacity*2)
	}
	limit := c.options.MaxRecordBytes * 4
	if payload > limit || uint64(capacity)*uint64(unsafe.Sizeof(jsonAggregateEntry{})) > limit-payload {
		return resource("JSON aggregate keys exceed state byte limit", model.ErrValueBudget)
	}
	if err := c.check(ctx); err != nil {
		return err
	}
	if !found {
		if err := c.insertJSONEntrySlot(ctx, state, index, capacity); err != nil {
			return err
		}
	}
	state.entries[index], state.entryBytes = entry, payload
	return nil
}

func (c *cursor) insertJSONEntrySlot(ctx context.Context, state *jsonAggregateState, index, capacity int) error {
	const chunk = 256
	if capacity != cap(state.entries) {
		next := make([]jsonAggregateEntry, len(state.entries), capacity)
		for start := 0; start < len(state.entries); start += chunk {
			if err := c.check(ctx); err != nil {
				return err
			}
			end := min(start+chunk, len(state.entries))
			copy(next[start:end], state.entries[start:end])
		}
		state.entries = next
	}
	state.entries = state.entries[:len(state.entries)+1]
	for end := len(state.entries) - 1; end > index; {
		if err := c.check(ctx); err != nil {
			return err
		}
		start := max(index, end-chunk)
		copy(state.entries[start+1:end+1], state.entries[start:end])
		end = start
	}
	return nil
}

func (c *cursor) advanceJSONAggregate(ctx context.Context, call compiledCall, transition *transition, arguments []model.Value) error {
	p := call.json
	if transition.json == nil {
		framing := 2
		if p.object && !p.binary {
			framing = 4
		}
		transition.json = &jsonAggregateState{framing: framing}
	}
	s := transition.json
	transition.count++
	valueIndex := 0
	if p.object {
		valueIndex = 1
		if arguments[0].IsNull {
			if p.binary {
				return &model.Error{Code: "22023", Message: "field name must not be null"}
			}
			return &model.Error{Code: "22004", Message: "null value not allowed for object key"}
		}
	}
	skip := p.strict && arguments[valueIndex].IsNull
	if skip && (!p.object || !p.unique) {
		return nil
	}
	var key string
	index, found := 0, false
	if p.object {
		encoded, err := p.encoders[0].EncodeKey(ctx, arguments[0], c.jsonBudget())
		if err != nil {
			return err
		}
		if p.binary {
			encoded.StrVal, err = builtin.CanonicalJSONB(ctx, encoded.StrVal, c.jsonBudget())
			if err != nil {
				return err
			}
		}
		key = encoded.StrVal
		if p.binary || p.unique {
			index, found, err = c.jsonEntry(ctx, s, key)
			if err != nil {
				return err
			}
			if found && p.unique && !p.binary {
				return &model.Error{Code: "22030", Message: "duplicate JSON object key value: " + key}
			}
		}
	}
	var value string
	if !skip {
		encoded, err := p.encoders[valueIndex].EncodeValue(ctx, arguments[valueIndex], builtin.JSONCompact, c.jsonBudget())
		if err != nil {
			return err
		}
		if p.binary {
			encoded.StrVal, err = builtin.CanonicalJSONB(ctx, encoded.StrVal, c.jsonBudget())
			if err != nil {
				return err
			}
		}
		value = encoded.StrVal
	}
	if p.object && (p.binary || p.unique) {
		entry := jsonAggregateEntry{key: key, skip: skip}
		if p.binary {
			entry.value = value
		}
		if found && p.unique {
			s.duplicate = true
		}
		if err := c.putJSONEntry(ctx, s, index, found, entry); err != nil {
			return err
		}
		if p.binary || skip {
			return nil
		}
	}
	separator := ""
	if s.emitted {
		separator = ", "
		if !p.object && !p.binary && p.structured && !arguments[0].IsNull {
			separator += "\n "
		}
	}
	if p.object {
		if err := c.appendJSON(ctx, s, separator, key, " : ", value); err != nil {
			return err
		}
	} else if err := c.appendJSON(ctx, s, separator, value); err != nil {
		return err
	}
	s.emitted = true
	return nil
}

func (c *cursor) finishJSONAggregate(ctx context.Context, call compiledCall, transition *transition) (model.Value, error) {
	s, p := transition.json, call.json
	if s == nil {
		return model.NullVal(model.TypeJSONB), nil
	}
	if s.duplicate {
		return model.Value{}, &model.Error{Code: "22030", Message: "duplicate JSON object key value"}
	}
	body := s.buffer
	if p.binary && p.object {
		assembled := jsonAggregateState{framing: 2}
		for _, entry := range s.entries {
			if err := c.check(ctx); err != nil {
				return model.Value{}, err
			}
			if entry.skip {
				continue
			}
			separator := ""
			if assembled.emitted {
				separator = ", "
			}
			if err := c.appendJSON(ctx, &assembled, separator, entry.key, ": ", entry.value); err != nil {
				return model.Value{}, err
			}
			assembled.emitted = true
		}
		body = assembled.buffer
	}
	open, close := "[", "]"
	if p.object {
		open, close = "{ ", " }"
		if p.binary {
			open, close = "{", "}"
		}
	}
	var output strings.Builder
	output.Grow(len(open) + len(body) + len(close))
	output.WriteString(open)
	for offset := 0; offset < len(body); offset += stringAggregateCopyBytes {
		if err := c.check(ctx); err != nil {
			return model.Value{}, err
		}
		output.Write(body[offset:min(offset+stringAggregateCopyBytes, len(body))])
	}
	output.WriteString(close)
	if p.binary && p.object {
		text, err := builtin.CanonicalJSONB(ctx, output.String(), c.jsonBudget())
		if err != nil {
			return model.Value{}, err
		}
		return model.Value{Type: model.TypeJSONB, StrVal: text}, nil
	}
	return model.Value{Type: model.TypeJSONB, StrVal: output.String()}, nil
}

func (g *group) clearJSON() {
	for i := range g.states {
		if state := g.states[i].json; state != nil {
			clear(state.entries)
			*state = jsonAggregateState{}
			g.states[i].json = nil
		}
	}
}
