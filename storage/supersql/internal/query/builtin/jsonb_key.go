package builtin

import (
	"context"
	"encoding/binary"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// Every retained node contributes at least one encoded kind byte. Its bounded
// parser workspace additionally covers one frame and decoded text per key byte.
func jsonbKeyWorkspace(maximum uint64) int64 {
	factor := uint64(unsafe.Sizeof(jsonbNode{})) + uint64(unsafe.Sizeof(jsonbFrame{})) + 1
	if maximum > uint64(math.MaxInt64)/factor {
		return math.MaxInt64
	}
	return int64(maximum * factor)
}

type jsonbKeyIterator struct {
	document *jsonbDocument
	current  int
	depth    int
}

func (iterator *jsonbKeyIterator) next() (int, int, bool) {
	if iterator.current < 0 {
		return 0, 0, false
	}
	index, parent := iterator.current, -1
	if iterator.depth != 0 {
		parent = iterator.document.frames[iterator.depth-1].parent
	}
	node := &iterator.document.nodes[index]
	if node.child >= 0 {
		iterator.document.frames[iterator.depth].parent = index
		iterator.depth++
		iterator.current = node.child
		return index, parent, true
	}
	iterator.current = node.next
	for iterator.current < 0 && iterator.depth != 0 {
		iterator.depth--
		container := iterator.document.frames[iterator.depth].parent
		iterator.current = iterator.document.nodes[container].next
	}
	return index, parent, true
}

func prepareJSONBKey(ctx context.Context, text string, maximum uint64) ([]byte, error) {
	if maximum == 0 {
		return nil, columnar.ErrBatchBudget
	}
	document, err := parseJSONBDocument(ctx, text, jsonbKeyWorkspace(maximum))
	if err != nil {
		return nil, err
	}
	// PostgreSQL's raw-scalar wrapper makes an empty root array sort first.
	if document.nodes[0].kind == jsonTextArray && document.nodes[0].child < 0 {
		return []byte{0}, nil
	}
	size := uint64(1)
	var numerics []numericKey
	iterator := jsonbKeyIterator{document: &document}
	for {
		index, parent, ok := iterator.next()
		if !ok {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		node := &document.nodes[index]
		if parent >= 0 && document.nodes[parent].kind == jsonTextObject {
			if err := addJSONBKeyBytes(&size, uint64(len(node.key))+3, maximum); err != nil {
				return nil, err
			}
		}
		if err := addJSONBKeyBytes(&size, 1, maximum); err != nil {
			return nil, err
		}
		var payload uint64
		switch node.kind {
		case jsonTextNull:
		case jsonTextBoolean:
			payload = 1
		case jsonTextString:
			payload = uint64(len(node.text)) + 2
		case jsonTextNumber:
			prepared, bytes, err := prepareNumericKey(ctx, node.text, maximum-size+1)
			if err != nil {
				return nil, err
			}
			numerics = append(numerics, prepared)
			payload = bytes - 1
		case jsonTextArray, jsonTextObject:
			payload = 4
		default:
			return nil, storage.ErrInvalidEncoding
		}
		if err := addJSONBKeyBytes(&size, payload, maximum); err != nil {
			return nil, err
		}
	}
	output := make([]byte, 0, int(size))
	output = append(output, 1)
	iterator = jsonbKeyIterator{document: &document}
	numericIndex := 0
	for {
		index, parent, ok := iterator.next()
		if !ok {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		node := &document.nodes[index]
		if parent >= 0 && document.nodes[parent].kind == jsonTextObject {
			output, err = appendJSONBKeyString(ctx, output, node.key)
			if err != nil {
				return nil, err
			}
		}
		output = append(output, byte(jsonbKindOrder(node.kind)))
		switch node.kind {
		case jsonTextString:
			output, err = appendJSONBKeyText(ctx, output, node.text)
		case jsonTextNumber:
			output, err = appendNumericKey(ctx, output, numerics[numericIndex], false)
			numericIndex++
		case jsonTextBoolean:
			bit := byte(0)
			if node.text == "true" {
				bit = 1
			}
			output = append(output, bit)
		case jsonTextArray, jsonTextObject:
			var count int
			count, err = jsonbChildCount(ctx, document.nodes, index)
			if err == nil {
				output = binary.BigEndian.AppendUint32(output, uint32(count))
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if uint64(len(output)) != size {
		return nil, storage.ErrInvalidEncoding
	}
	return output, ctx.Err()
}

func addJSONBKeyBytes(size *uint64, bytes, maximum uint64) error {
	if bytes > maximum-*size {
		return columnar.ErrBatchBudget
	}
	*size += bytes
	return nil
}

func appendJSONBKeyString(ctx context.Context, output []byte, text string) ([]byte, error) {
	output = append(output, byte(jsonbKindOrder(jsonTextString)))
	return appendJSONBKeyText(ctx, output, text)
}

func appendJSONBKeyText(ctx context.Context, output []byte, text string) ([]byte, error) {
	for i := range len(text) {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		// JSONB rejects U+0000, so decoded strings need only a terminator.
		output = append(output, text[i])
	}
	return append(output, 0, 0), nil
}

func appendPreparedJSONBKey(ctx context.Context, key, payload []byte, mask byte) ([]byte, error) {
	for i, value := range payload {
		if i%cancellationChunk == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		key = append(key, value^mask)
	}
	return key, nil
}
