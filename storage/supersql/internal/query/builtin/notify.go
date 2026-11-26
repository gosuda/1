package builtin

import (
	"context"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	// MaxNotifyChannelBytes matches PostgreSQL's NAMEDATALEN - 1 identifier limit.
	MaxNotifyChannelBytes = 63
	// MaxNotifyPayloadBytes matches PostgreSQL's NOTIFY_PAYLOAD_MAX_LENGTH (8000).
	MaxNotifyPayloadBytes = 8000
)

// NotifyRuntime provides session-scoped PostgreSQL LISTEN/NOTIFY callbacks for
// pg_notify(text, text) and pg_listening_channels().
type NotifyRuntime interface {
	Notify(channel, payload string) error
	ListeningChannels() []string
}

type notifyRuntimeKey struct{}

// WithNotifyRuntime attaches a session NotifyRuntime to ctx.
func WithNotifyRuntime(ctx context.Context, runtime NotifyRuntime) context.Context {
	if runtime == nil {
		return ctx
	}
	return context.WithValue(ctx, notifyRuntimeKey{}, runtime)
}

// NotifyRuntimeFromContext extracts the session NotifyRuntime from ctx, if any.
func NotifyRuntimeFromContext(ctx context.Context) NotifyRuntime {
	if ctx == nil {
		return nil
	}
	rt, _ := ctx.Value(notifyRuntimeKey{}).(NotifyRuntime)
	return rt
}

// ValidateNotifyChannel validates a PostgreSQL LISTEN/NOTIFY channel name.
func ValidateNotifyChannel(channel string) (string, error) {
	ch := strings.TrimSpace(channel)
	if ch == "" {
		return "", sqlError("22023", "channel name cannot be empty")
	}
	if len(ch) > MaxNotifyChannelBytes {
		return "", sqlError("22023", "channel name too long")
	}
	return strings.ToLower(ch), nil
}

// ValidateNotifyPayload validates a PostgreSQL NOTIFY payload byte length.
func ValidateNotifyPayload(payload string) error {
	if len(payload) > MaxNotifyPayloadBytes {
		return sqlError("54000", "payload string too long")
	}
	return nil
}

func compileNotify(signature Signature) scalarKernel {
	if signature.ID != 3036 {
		return nil
	}
	return func(ctx context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) {
		if len(arguments) != 2 {
			return model.Value{}, sqlError("42883", "pg_notify requires two arguments")
		}
		if arguments[0].IsNull {
			return model.Value{}, sqlError("22023", "channel name cannot be empty")
		}
		channel, err := ValidateNotifyChannel(arguments[0].StrVal)
		if err != nil {
			return model.Value{}, err
		}
		payload := ""
		if !arguments[1].IsNull {
			payload = arguments[1].StrVal
		}
		if err := ValidateNotifyPayload(payload); err != nil {
			return model.Value{}, err
		}
		runtime := NotifyRuntimeFromContext(ctx)
		if runtime == nil {
			return model.Value{}, sqlError("55000", "notification runtime is unavailable")
		}
		if err := runtime.Notify(channel, payload); err != nil {
			return model.Value{}, err
		}
		return model.StringVal(""), nil
	}
}

type notifyRowsPlan struct {
	metadata uint64
}

var _ RowSourcePlan = (*notifyRowsPlan)(nil)

// ResolveNotifyRows resolves pg_listening_channels() (OID 3035).
func ResolveNotifyRows(ctx context.Context, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, _ ResolveOptions, maximum uint64) (RowSourcePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	signature, known := LookupSignature(id)
	if !known {
		return nil, &UnknownFunctionError{ID: id}
	}
	if id != 3035 {
		return nil, &MissingImplementationError{Signature: signature}
	}
	if len(arguments) != 0 {
		return nil, sqlError("42883", "pg_listening_channels accepts no arguments")
	}
	if result.Type.ID != 25 || result.Type.Storage != model.TypeString {
		return nil, sqlError("42804", "pg_listening_channels requires text result layout")
	}
	metadata := uint64(unsafe.Sizeof(notifyRowsPlan{}))
	if metadata > maximum {
		return nil, sqlError("54000", "builtin type metadata budget exceeded")
	}
	return &notifyRowsPlan{metadata: metadata}, nil
}

func (p *notifyRowsPlan) OwnedBytes() uint64 { return p.metadata }

func (p *notifyRowsPlan) Open(ctx context.Context, arguments []model.Value, _ RowSourceOptions) (RowSourceIterator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(arguments) != 0 {
		return nil, sqlError("42883", "pg_listening_channels accepts no arguments")
	}
	var channels []string
	if runtime := NotifyRuntimeFromContext(ctx); runtime != nil {
		channels = runtime.ListeningChannels()
	}
	return &notifyRowsIterator{
		channels: channels,
	}, nil
}

type notifyRowsIterator struct {
	channels []string
	pos      int
	closed   bool
}

var _ RowSourceIterator = (*notifyRowsIterator)(nil)

func (it *notifyRowsIterator) Next(ctx context.Context) ([]model.Value, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if it.closed || it.pos >= len(it.channels) {
		return nil, false, nil
	}
	ch := it.channels[it.pos]
	it.pos++
	return []model.Value{model.StringVal(ch)}, true, nil
}

func (it *notifyRowsIterator) Close() error {
	it.closed = true
	it.channels = nil
	return nil
}
