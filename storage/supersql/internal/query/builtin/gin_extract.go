package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// GINSupportedType reports whether a column type supports GIN inverted indexing
// and returns the default operator class name when opClass is empty.
func GINSupportedType(layout model.TypeLayout, opClass string) (string, bool) {
	oc := strings.ToLower(strings.TrimSpace(opClass))
	switch {
	case layout.Type.ID == 3802: // jsonb
		if oc == "" || oc == "jsonb_ops" {
			return "jsonb_ops", true
		}
		if oc == "jsonb_path_ops" {
			return "jsonb_path_ops", true
		}
		return "", false
	case layout.Type.ID == 3614: // tsvector
		if oc == "" || oc == "tsvector_ops" {
			return "tsvector_ops", true
		}
		return "", false
	case layout.Type.Storage == model.TypeArray && len(layout.Children) == 1:
		if oc == "" || oc == "array_ops" {
			return "array_ops", true
		}
		return "", false
	default:
		return "", false
	}
}

// ExtractGINDocumentTokens extracts deduplicated, sorted inverted-index tokens
// from a single document value for a GIN index column.
func ExtractGINDocumentTokens(ctx context.Context, layout model.TypeLayout, val model.Value, opClass string, encoder storage.KeyEncoder, maxBytes uint64) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if val.IsNull {
		return nil, nil
	}
	resolvedOpClass, ok := GINSupportedType(layout, opClass)
	if !ok {
		return nil, sqlError("42704", "data type has no default operator class for access method \"gin\"")
	}
	seen := make(map[string]struct{})
	var totalBytes uint64
	addToken := func(tok []byte) error {
		if len(tok) == 0 {
			return nil
		}
		key := string(tok)
		if _, dup := seen[key]; dup {
			return nil
		}
		if maxBytes > 0 && totalBytes+uint64(len(tok)) > maxBytes {
			return sqlError("54000", "GIN index entry token budget exceeded")
		}
		seen[key] = struct{}{}
		totalBytes += uint64(len(tok))
		return nil
	}

	switch resolvedOpClass {
	case "jsonb_ops", "jsonb_path_ops":
		var doc any
		dec := json.NewDecoder(strings.NewReader(val.StrVal))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return nil, sqlError("22P02", "invalid jsonb value for GIN extraction")
		}
		pathOnly := resolvedOpClass == "jsonb_path_ops"
		if err := extractJSONBTokens(ctx, doc, "", pathOnly, true, addToken, 0); err != nil {
			return nil, err
		}
	case "tsvector_ops":
		lexemes, err := decodeTSVector(ctx, val.BytesVal)
		if err != nil {
			return nil, err
		}
		for _, lex := range lexemes {
			if err := addToken([]byte("t:" + lex.text)); err != nil {
				return nil, err
			}
		}
	case "array_ops":
		if encoder == nil {
			return nil, sqlError("XX000", "GIN array extraction requires key encoder")
		}
		elemLayout := layout.Children[0]
		elemCols := []storage.KeyColumn{{ColumnID: 1, Type: elemLayout.Type, Children: elemLayout.Children}}
		for _, elem := range val.ArrayVal {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if elem.IsNull {
				continue
			}
			enc, err := encoder.EncodePrefix(ctx, elemCols, []model.Value{elem}, max(maxBytes, 64<<10))
			if err != nil {
				return nil, err
			}
			tok := make([]byte, 2+len(enc))
			tok[0], tok[1] = 'a', ':'
			copy(tok[2:], enc)
			if err := addToken(tok); err != nil {
				return nil, err
			}
		}
	}

	out := make([][]byte, 0, len(seen))
	for tok := range seen {
		out = append(out, []byte(tok))
	}
	slices.SortFunc(out, bytes.Compare)
	return out, nil
}

// GINQueryPlan describes the inverted index lookup tokens derived from a query predicate.
type GINQueryPlan struct {
	Tokens     [][]byte
	RequireAll bool // true = intersect postings, false = union postings
	Exact      bool // false => always recheck predicate on fetched base rows
}

// ExtractGINQueryPlan extracts GIN lookup tokens for a binary predicate
// `indexed_col <op> const_rhs`. Returns (plan, true, nil) when the GIN index
// can prune candidate rows safely (false recall is never permitted).
func ExtractGINQueryPlan(ctx context.Context, operatorID model.OperatorID, colLayout model.TypeLayout, rhs model.Value, opClass string, encoder storage.KeyEncoder, maxBytes uint64) (GINQueryPlan, bool, error) {
	if rhs.IsNull {
		return GINQueryPlan{}, false, nil
	}
	resolvedOpClass, ok := GINSupportedType(colLayout, opClass)
	if !ok {
		return GINQueryPlan{}, false, nil
	}
	switch resolvedOpClass {
	case "jsonb_ops", "jsonb_path_ops":
		switch operatorID {
		case 3246: // jsonb @> jsonb
			tokens, err := ExtractGINDocumentTokens(ctx, colLayout, rhs, resolvedOpClass, encoder, maxBytes)
			if err != nil || len(tokens) == 0 {
				return GINQueryPlan{}, false, err
			}
			return GINQueryPlan{Tokens: tokens, RequireAll: true, Exact: false}, true, nil
		case 3247: // jsonb ? text
			if resolvedOpClass == "jsonb_path_ops" {
				return GINQueryPlan{}, false, nil
			}
			return GINQueryPlan{Tokens: [][]byte{[]byte("k:" + rhs.StrVal)}, RequireAll: true, Exact: false}, true, nil
		case 3248, 3249: // jsonb ?| text[] (any), jsonb ?& text[] (all)
			if resolvedOpClass == "jsonb_path_ops" {
				return GINQueryPlan{}, false, nil
			}
			tokens := make([][]byte, 0, len(rhs.ArrayVal))
			seen := make(map[string]struct{}, len(rhs.ArrayVal))
			for _, elem := range rhs.ArrayVal {
				if elem.IsNull {
					continue
				}
				tok := "k:" + elem.StrVal
				if _, dup := seen[tok]; dup {
					continue
				}
				seen[tok] = struct{}{}
				tokens = append(tokens, []byte(tok))
			}
			if len(tokens) == 0 {
				return GINQueryPlan{}, false, nil
			}
			slices.SortFunc(tokens, bytes.Compare)
			return GINQueryPlan{Tokens: tokens, RequireAll: operatorID == 3249, Exact: false}, true, nil
		}
	case "array_ops":
		switch operatorID {
		case 2751, 2750: // array @> array (2751), array && array (2750)
			tokens, err := ExtractGINDocumentTokens(ctx, colLayout, rhs, resolvedOpClass, encoder, maxBytes)
			if err != nil || len(tokens) == 0 {
				return GINQueryPlan{}, false, err
			}
			return GINQueryPlan{Tokens: tokens, RequireAll: operatorID == 2751, Exact: false}, true, nil
		}
	case "tsvector_ops":
		if operatorID == 3636 || operatorID == 3637 || operatorID == 3660 || operatorID == 3661 {
			root, _, err := decodeTSQuery(ctx, rhs.BytesVal)
			if err != nil || root == nil {
				return GINQueryPlan{}, false, err
			}
			tokens, requireAll, usable := extractTSQueryGINTokens(root)
			if !usable || len(tokens) == 0 {
				return GINQueryPlan{}, false, nil
			}
			slices.SortFunc(tokens, bytes.Compare)
			return GINQueryPlan{Tokens: tokens, RequireAll: requireAll, Exact: false}, true, nil
		}
	}
	return GINQueryPlan{}, false, nil
}

func extractTSQueryGINTokens(node *tsQueryNode) ([][]byte, bool, bool) {
	if node == nil {
		return nil, false, false
	}
	switch node.kind {
	case tsQueryValue:
		if node.prefix || node.word == "" {
			return nil, false, false
		}
		return [][]byte{[]byte("t:" + node.word)}, true, true
	case tsQueryOp:
		switch node.op {
		case tsQueryAnd, tsQueryPhrase:
			lToks, lAll, lOK := extractTSQueryGINTokens(node.left)
			rToks, rAll, rOK := extractTSQueryGINTokens(node.right)
			if lOK && lAll && rOK && rAll {
				return dedupTokenSlices(lToks, rToks), true, true
			}
			if lOK && lAll {
				return lToks, true, true
			}
			if rOK && rAll {
				return rToks, true, true
			}
			return nil, false, false
		case tsQueryOr:
			lToks, _, lOK := extractTSQueryGINTokens(node.left)
			rToks, _, rOK := extractTSQueryGINTokens(node.right)
			if lOK && rOK {
				return dedupTokenSlices(lToks, rToks), false, true
			}
			return nil, false, false
		case tsQueryNot:
			return nil, false, false
		}
	}
	return nil, false, false
}

func dedupTokenSlices(a, b [][]byte) [][]byte {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([][]byte, 0, len(a)+len(b))
	for _, s := range a {
		k := string(s)
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			out = append(out, s)
		}
	}
	for _, s := range b {
		k := string(s)
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func extractJSONBTokens(ctx context.Context, val any, path string, pathOnly bool, topLevel bool, add func([]byte) error, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 {
		return sqlError("54000", "jsonb nesting depth exceeded during GIN extraction")
	}
	switch v := val.(type) {
	case map[string]any:
		for k, child := range v {
			if !pathOnly {
				if err := add([]byte("k:" + k)); err != nil {
					return err
				}
			}
			nextPath := k
			if path != "" {
				nextPath = path + "\x1f" + k
			}
			if err := extractJSONBTokens(ctx, child, nextPath, pathOnly, false, add, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, elem := range v {
			// PostgreSQL JSONB ?/?|/?& match direct string elements of a top-level array.
			if !pathOnly && topLevel {
				if s, ok := elem.(string); ok {
					if err := add([]byte("k:" + s)); err != nil {
						return err
					}
				}
			}
			// Array elements share the enclosing path so @> containment matches any element.
			if err := extractJSONBTokens(ctx, elem, path, pathOnly, false, add, depth+1); err != nil {
				return err
			}
		}
	default:
		// PostgreSQL JSONB ?/?|/?& also match a top-level JSONB string scalar.
		if !pathOnly && topLevel {
			if s, ok := v.(string); ok {
				if err := add([]byte("k:" + s)); err != nil {
					return err
				}
			}
		}
		scalar := canonicalJSONBScalar(v)
		if !pathOnly {
			if err := add([]byte("v:" + scalar)); err != nil {
				return err
			}
		}
		if path != "" {
			if err := add([]byte("kv:" + path + "=" + scalar)); err != nil {
				return err
			}
		} else if pathOnly {
			if err := add([]byte("v:" + scalar)); err != nil {
				return err
			}
		}
	}
	return nil
}

func canonicalJSONBScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "b:true"
		}
		return "b:false"
	case string:
		return "s:" + x
	case json.Number:
		// Canonicalize equivalent numbers like 1.0 and 1.
		if f, err := strconv.ParseFloat(x.String(), 64); err == nil {
			return "n:" + strconv.FormatFloat(f, 'g', -1, 64)
		}
		return "n:" + x.String()
	default:
		return fmt.Sprintf("u:%v", x)
	}
}
