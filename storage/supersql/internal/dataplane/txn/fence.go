package txn

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// FenceAuthority authenticates database-local closed-read barriers. Root
// composition owns its persisted secret and injects only FenceVerifier into
// storage. Authentication does not replace the sequencer's certified clock wait.
type FenceAuthority struct{ secret [32]byte }

func NewFenceAuthority(secret [32]byte) (*FenceAuthority, error) {
	if secret == ([32]byte{}) {
		return nil, storage.ErrSnapshotNotClosed
	}
	return &FenceAuthority{secret: secret}, nil
}

// Issue is called only after all decisions through position are published and
// the certified closed timestamp barrier has completed in exactly this epoch.
// Position zero closes the empty log; it does not require a dummy transaction.
func (a *FenceAuthority) Issue(ctx context.Context, closed model.Timestamp, position uint64, epoch clock.Epoch) (storage.ReadFence, error) {
	if err := ctx.Err(); err != nil {
		return storage.ReadFence{}, err
	}
	if closed <= 0 {
		return storage.ReadFence{}, storage.ErrSnapshotNotClosed
	}
	fence := storage.ReadFence{ClosedTS: closed, LogPosition: position, Epoch: epoch}
	fence.ID = a.tag(fence)
	if fence.ID == ([16]byte{}) {
		return storage.ReadFence{}, storage.ErrSnapshotNotClosed
	}
	return fence, nil
}

func (a *FenceAuthority) VerifyReadFence(ctx context.Context, fence storage.ReadFence) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := (storage.SnapshotSpec{Timestamp: fence.ClosedTS, Fence: fence}).Validate(); err != nil {
		return err
	}
	expected := a.tag(fence)
	if !hmac.Equal(expected[:], fence.ID[:]) {
		return storage.ErrSnapshotNotClosed
	}
	return nil
}

func (a *FenceAuthority) tag(fence storage.ReadFence) [16]byte {
	const domain = "supersql/closed-read-fence/v1"
	var data [len(domain) + 96]byte
	copy(data[:], domain)
	payload := data[len(domain):]
	binary.LittleEndian.PutUint64(payload, uint64(fence.ClosedTS))
	binary.LittleEndian.PutUint64(payload[8:], fence.LogPosition)
	copy(payload[16:], fence.Epoch.Assurance[:])
	copy(payload[32:], fence.Epoch.LeapHistory[:])
	copy(payload[64:], fence.Epoch.Config[:])
	digest := hmac.New(sha256.New, a.secret[:])
	digest.Write(data[:])
	var sum [32]byte
	digest.Sum(sum[:0])
	return [16]byte(sum[:16])
}
