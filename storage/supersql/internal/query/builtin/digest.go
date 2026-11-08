package builtin

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"hash"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileDigest(signature Signature) scalarKernel {
	var newDigest func() hash.Hash
	var outputBytes int
	switch signature.ID {
	case 2311, 2321:
		newDigest, outputBytes = md5.New, md5.Size*2
	case 3419:
		newDigest, outputBytes = sha256.New224, sha256.Size224
	case 3420:
		newDigest, outputBytes = sha256.New, sha256.Size
	case 3421:
		newDigest, outputBytes = sha512.New384, sha512.Size384
	case 3422:
		newDigest, outputBytes = sha512.New, sha512.Size
	default:
		return nil
	}
	textInput := signature.ID == 2311
	hexOutput := signature.ID == 2311 || signature.ID == 2321
	return func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		if textInput {
			if _, err := textCharacters(ctx, args[0].StrVal); err != nil {
				return model.Value{}, err
			}
		}
		if err := outputBudget(int64(outputBytes), budget); err != nil {
			return model.Value{}, err
		}
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		digest := newDigest()
		var err error
		if textInput {
			err = writeDigestText(ctx, digest, args[0].StrVal)
		} else {
			err = writeDigestBytes(ctx, digest, args[0].BytesVal)
		}
		if err != nil {
			return model.Value{}, err
		}
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		if hexOutput {
			var sum [md5.Size]byte
			digest.Sum(sum[:0])
			var output [md5.Size * 2]byte
			hex.Encode(output[:], sum[:])
			return model.StringVal(string(output[:])), nil
		}
		return model.BytesVal(digest.Sum(make([]byte, 0, outputBytes))), nil
	}
}

func writeDigestText(ctx context.Context, digest hash.Hash, input string) error {
	var scratch [cancellationChunk]byte
	for start := 0; start < len(input); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+cancellationChunk, len(input))
		n := copy(scratch[:], input[start:end])
		if _, err := digest.Write(scratch[:n]); err != nil {
			return err
		}
	}
	return nil
}

func writeDigestBytes(ctx context.Context, digest hash.Hash, input []byte) error {
	for start := 0; start < len(input); start += cancellationChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+cancellationChunk, len(input))
		if _, err := digest.Write(input[start:end]); err != nil {
			return err
		}
	}
	return nil
}
