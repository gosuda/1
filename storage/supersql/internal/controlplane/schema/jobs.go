package schema

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type JobID uint64
type JobKind uint8
type JobState uint8

const (
	AddIndexJob JobKind = iota + 1
	DropIndexJob
	AddColumnJob
	RewriteGenerationJob
)
const (
	JobPending JobState = iota + 1
	JobDeleteOnly
	JobWriteOnly
	JobBackfilling
	JobValidating
	JobPublic
	JobSucceeded
	JobReverting
	JobFailed
	JobCanceled
)

// Job owns resume/failure keys. SourceVersion and TargetVersion identify the
// immutable transformation contract, while Revision serializes durable progress.
type Job struct {
	ID                                     JobID
	TableID                                model.ObjectID
	MutationID                             model.ObjectID
	Kind                                   JobKind
	State                                  JobState
	SourceVersion, TargetVersion, Revision uint64
	SnapshotTS, ProtectedTimestamp         model.Timestamp
	ResumeKey, FailureKey                  []byte
	ErrorCode                              string
	Attempt                                uint64
}

func (j Job) Terminal() bool {
	return j.State == JobSucceeded || j.State == JobFailed || j.State == JobCanceled
}

var ErrJobConflict = &model.Error{Code: "55006", Message: "table already has an active schema mutation job"}
var ErrJobChanged = &model.Error{Code: "40001", Message: "schema job revision changed"}
var ErrJobTransition = &model.Error{Code: "55000", Message: "invalid schema job state transition"}
var ErrJobRecord = errors.New("invalid durable schema job record")
var ErrJobLimit = &model.Error{Code: "53200", Message: "schema job resource limit exceeded"}

const jobHeaderBytes = 96
const jobEncoding = "SJOB0001"

func validJob(job Job, maximum uint64) error {
	missingIdentity := job.ID == 0 || job.TableID == 0 || job.MutationID == 0
	missingVersion := job.SourceVersion == 0 || job.TargetVersion == 0 || job.Revision == 0
	if missingIdentity || missingVersion {
		return ErrJobRecord
	}
	if job.Kind < AddIndexJob || job.Kind > RewriteGenerationJob || job.State < JobPending || job.State > JobCanceled {
		return ErrJobRecord
	}
	if job.SnapshotTS < 0 || job.ProtectedTimestamp < 0 {
		return ErrJobRecord
	}
	if job.State >= JobBackfilling && job.State <= JobPublic && job.SnapshotTS == 0 {
		return ErrJobRecord
	}
	if job.ProtectedTimestamp != 0 && job.ProtectedTimestamp != job.SnapshotTS {
		return ErrJobRecord
	}
	if job.SnapshotTS != 0 && !job.Terminal() && job.ProtectedTimestamp == 0 {
		return ErrJobRecord
	}
	if job.Terminal() && job.ProtectedTimestamp != 0 {
		return ErrJobRecord
	}
	failing := job.State == JobReverting || job.State == JobFailed || job.State == JobCanceled
	if failing != (job.ErrorCode != "") {
		return ErrJobRecord
	}
	if job.ErrorCode != "" {
		if len(job.ErrorCode) != 5 {
			return ErrJobRecord
		}
		for _, character := range job.ErrorCode {
			digit := character >= '0' && character <= '9'
			upper := character >= 'A' && character <= 'Z'
			if !digit && !upper {
				return ErrJobRecord
			}
		}
	}
	if job.State == JobCanceled && job.ErrorCode != "57014" {
		return ErrJobRecord
	}
	if uint64(len(job.ResumeKey)) > math.MaxUint32 || uint64(len(job.FailureKey)) > math.MaxUint32 {
		return ErrJobLimit
	}
	bytes := uint64(jobHeaderBytes) + uint64(len(job.ResumeKey)) + uint64(len(job.FailureKey))
	if bytes > maximum || bytes > uint64(math.MaxInt) {
		return ErrJobLimit
	}
	return nil
}

func validateJobSuccessor(previous, next Job) error {
	sameIdentity := previous.ID == next.ID && previous.TableID == next.TableID && previous.MutationID == next.MutationID && previous.Kind == next.Kind
	sameTransform := previous.SourceVersion == next.SourceVersion && previous.TargetVersion == next.TargetVersion
	if !sameIdentity || !sameTransform || previous.Revision == math.MaxUint64 || next.Revision != previous.Revision+1 || previous.Terminal() {
		return ErrJobTransition
	}
	if next.Attempt < previous.Attempt || next.Attempt-previous.Attempt > 1 {
		return ErrJobTransition
	}
	if previous.SnapshotTS != 0 && next.SnapshotTS != previous.SnapshotTS {
		return ErrJobTransition
	}
	if previous.SnapshotTS == 0 && next.SnapshotTS != 0 && previous.State != JobWriteOnly {
		return ErrJobTransition
	}
	if !bytes.Equal(previous.ResumeKey, next.ResumeKey) {
		finishing := next.State == JobBackfilling || next.State == JobValidating
		if previous.State != JobBackfilling || !finishing || bytes.Compare(next.ResumeKey, previous.ResumeKey) <= 0 {
			return ErrJobTransition
		}
	}
	if previous.State == next.State {
		if previous.State == JobPublic || previous.State == JobReverting {
			if next.Attempt == previous.Attempt {
				return ErrJobTransition
			}
		}
		return nil
	}
	if next.State == JobReverting && previous.State < JobPublic {
		return nil
	}
	switch previous.State {
	case JobPending:
		if next.State == JobDeleteOnly {
			return nil
		}
	case JobDeleteOnly:
		if next.State == JobWriteOnly {
			return nil
		}
	case JobWriteOnly:
		if next.State == JobBackfilling {
			return nil
		}
	case JobBackfilling:
		if next.State == JobValidating {
			return nil
		}
	case JobValidating:
		if next.State == JobPublic {
			return nil
		}
	case JobPublic:
		if next.State == JobSucceeded {
			return nil
		}
	case JobReverting:
		if next.State == JobFailed || next.State == JobCanceled {
			return nil
		}
	}
	return ErrJobTransition
}

func EncodeJob(ctx context.Context, job Job, maximum uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validJob(job, maximum); err != nil {
		return nil, err
	}
	data := make([]byte, jobHeaderBytes+len(job.ResumeKey)+len(job.FailureKey))
	copy(data, jobEncoding)
	numbers := [9]uint64{uint64(job.ID), uint64(job.TableID), uint64(job.MutationID), job.SourceVersion, job.TargetVersion, job.Revision, uint64(job.SnapshotTS), uint64(job.ProtectedTimestamp), job.Attempt}
	for i, number := range numbers {
		binary.LittleEndian.PutUint64(data[8+i*8:], number)
	}
	data[80], data[81] = byte(job.State), byte(job.Kind)
	copy(data[82:87], job.ErrorCode)
	binary.LittleEndian.PutUint32(data[88:92], uint32(len(job.ResumeKey)))
	binary.LittleEndian.PutUint32(data[92:96], uint32(len(job.FailureKey)))
	for offset := 0; offset < len(job.ResumeKey); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(offset+4096, len(job.ResumeKey))
		copy(data[jobHeaderBytes+offset:], job.ResumeKey[offset:end])
		offset = end
	}
	for offset := 0; offset < len(job.FailureKey); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(offset+4096, len(job.FailureKey))
		copy(data[jobHeaderBytes+len(job.ResumeKey)+offset:], job.FailureKey[offset:end])
		offset = end
	}
	return data, nil
}

func DecodeJob(ctx context.Context, data []byte, maximum uint64) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	if uint64(len(data)) > maximum {
		return Job{}, ErrJobLimit
	}
	if len(data) < jobHeaderBytes || string(data[:8]) != jobEncoding || data[87] != 0 {
		return Job{}, ErrJobRecord
	}
	resume, failure := uint64(binary.LittleEndian.Uint32(data[88:92])), uint64(binary.LittleEndian.Uint32(data[92:96]))
	if resume+failure != uint64(len(data)-jobHeaderBytes) {
		return Job{}, ErrJobRecord
	}
	var numbers [9]uint64
	for i := range numbers {
		numbers[i] = binary.LittleEndian.Uint64(data[8+i*8:])
	}
	if numbers[6] > math.MaxInt64 || numbers[7] > math.MaxInt64 {
		return Job{}, ErrJobRecord
	}
	job := Job{ID: JobID(numbers[0]), TableID: model.ObjectID(numbers[1]), MutationID: model.ObjectID(numbers[2]), SourceVersion: numbers[3], TargetVersion: numbers[4], Revision: numbers[5], SnapshotTS: model.Timestamp(numbers[6]), ProtectedTimestamp: model.Timestamp(numbers[7]), Attempt: numbers[8], State: JobState(data[80]), Kind: JobKind(data[81])}
	code := data[82:87]
	if !bytes.Equal(code, []byte{0, 0, 0, 0, 0}) {
		job.ErrorCode = string(code)
	}
	job.ResumeKey = bytes.Clone(data[jobHeaderBytes : jobHeaderBytes+int(resume)])
	job.FailureKey = bytes.Clone(data[jobHeaderBytes+int(resume):])
	if err := validJob(job, maximum); err != nil {
		return Job{}, err
	}
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	return job, nil
}
