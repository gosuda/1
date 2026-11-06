package bind

import "github.com/gosuda/supersql-alpha/internal/model"

func (b *binder) rejectIdentityOwnerChange(sequence, table model.ObjectID, column model.ColumnID) error {
	if table == 0 || column == 0 {
		return nil
	}
	descriptor, err := b.declarationDescriptor(table)
	if err != nil {
		return err
	}
	spec, err := b.descriptorSpec(descriptor)
	if err != nil {
		return err
	}
	if spec.Table == nil {
		return bindError("XX001", "sequence owner is not a table", 0)
	}
	for _, declared := range spec.Table.Columns {
		if declared.ID == column && declared.IdentitySequence == sequence {
			return &SequenceOwnershipError{Cause: bindError("0A000", "cannot change ownership of identity sequence", 0)}
		}
	}
	return nil
}
