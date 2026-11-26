package txn

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var errHistoricalWriteTransaction = &model.Error{Code: "25006", Message: "AS OF SYSTEM TIME requires a read-only transaction"}
var errHistoricalAfterAccess = &model.Error{Code: "25001", Message: "AS OF SYSTEM TIME must precede transaction data access"}
var errHistoricalTimestamp = &model.Error{Code: "22023", Message: "historical transaction timestamp must be positive"}
var errHistoricalTimestampChanged = &model.Error{Code: "0A000", Message: "transaction cannot mix historical timestamps"}

// SetHistoricalSnapshot selects an exact timestamp before a read-only transaction
// acquires any data snapshot. It never changes a writable transaction's mode.
func (transaction *Transaction) SetHistoricalSnapshot(ctx context.Context, timestamp model.Timestamp) error {
	operation, finish, nested, err := transaction.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if nested {
		return errTransactionReentry
	}
	if err := transaction.active(operation); err != nil {
		return err
	}
	if timestamp <= 0 {
		return errHistoricalTimestamp
	}
	if transaction.mode == ReadWrite {
		return errHistoricalWriteTransaction
	}
	if transaction.dataAccessed.Load() || transaction.base != nil {
		return errHistoricalAfterAccess
	}
	if transaction.mode == HistoricalReadOnly && transaction.timestamp != timestamp {
		return errHistoricalTimestampChanged
	}
	transaction.mode = HistoricalReadOnly
	transaction.timestamp = timestamp
	return nil
}
