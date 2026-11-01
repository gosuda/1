package decimalint

import "errors"

// Checkpoint identifies allocations, not numeric state. Retaining selected new
// results never changes or invalidates allocations made before the checkpoint.
type Checkpoint struct {
	owner  *Workspace
	serial uint64
}

func (workspace *Workspace) Checkpoint() Checkpoint {
	if workspace == nil || workspace.closed {
		panic(ErrClosed)
	}
	return Checkpoint{owner: workspace, serial: workspace.serial}
}

func (workspace *Workspace) ReleaseAfter(checkpoint Checkpoint, keep ...*Int) error {
	if workspace == nil || workspace.closed {
		return ErrClosed
	}
	if checkpoint.owner != workspace || checkpoint.serial > workspace.serial {
		return ErrInvalid
	}
	for _, value := range keep {
		if value != nil && (value.owner != workspace || value.released) {
			return ErrInvalid
		}
	}
	var err error
	for value := workspace.head; value != nil && value.serial > checkpoint.serial; {
		next := value.next
		retained := false
		for _, candidate := range keep {
			if candidate == value {
				retained = true
				break
			}
		}
		if !retained {
			err = errors.Join(err, value.Close())
		}
		value = next
	}
	return err
}
