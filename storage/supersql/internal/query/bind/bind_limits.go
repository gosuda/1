package bind

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) checkAST(message protoreflect.Message, depth uint32, remaining *uint32, bytes *uint64) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if !message.IsValid() {
		return nil
	}
	if depth > b.options.Limits.MaxDepth || *remaining == 0 {
		return bindError("54000", "binding AST limit exceeded", 0)
	}
	*remaining--
	if ref, ok := message.Interface().(*zpgparse.ColumnRef); ok {
		if err := b.noteWholeRowReference(ref); err != nil {
			return err
		}
	}
	fields := message.Descriptor().Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		if !message.Has(field) {
			continue
		}
		if err := b.checkASTField(field, message.Get(field), depth, remaining, bytes); err != nil {
			return err
		}
	}
	return nil
}

func (b *binder) checkASTField(field protoreflect.FieldDescriptor, value protoreflect.Value, depth uint32, remaining *uint32, bytes *uint64) error {
	if !field.IsList() {
		return b.checkASTValue(field.Kind(), value, depth, remaining, bytes)
	}
	list := value.List()
	if uint64(list.Len()) > uint64(b.options.Limits.MaxOperands) {
		return bindError("54000", "binding operand limit exceeded", 0)
	}
	for i := range list.Len() {
		if err := b.checkASTValue(field.Kind(), list.Get(i), depth, remaining, bytes); err != nil {
			return err
		}
	}
	return nil
}

func (b *binder) checkASTValue(kind protoreflect.Kind, value protoreflect.Value, depth uint32, remaining *uint32, bytes *uint64) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	var length uint64
	switch kind {
	case protoreflect.MessageKind:
		return b.checkAST(value.Message(), depth+1, remaining, bytes)
	case protoreflect.StringKind:
		length = uint64(len(value.String()))
	case protoreflect.BytesKind:
		length = uint64(len(value.Bytes()))
	}
	if length > *bytes {
		return bindError("54000", "binding source payload limit exceeded", 0)
	}
	*bytes -= length
	return nil
}
// descriptorLimits derives descriptor limits from binding limits. Nested SQL
// values and types cap at depth 64, so the binding depth MUST be clamped: an
// unclamped copy is rejected outright by DescriptorLimits.Validate, which would
// report every catalog type as unresolved.
func descriptorLimits(limits ir.Limits) model.DescriptorLimits {
	return model.DescriptorLimits{
		MaxBytes: limits.MaxBytes,
		MaxItems: int(limits.MaxOperands),
		MaxDepth: int(min(limits.MaxDepth, 64)),
	}
}

func (b *binder) descriptorLimits() model.DescriptorLimits {
	return descriptorLimits(b.options.Limits)
}

func (b *binder) descriptorSpec(d *model.Descriptor) (model.DescriptorSpec, error) {
	return d.SpecWithin(b.ctx, b.descriptorLimits())
}
