package builtin

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)


func TestFullTextVectorParsingNormalizesLexemesPositionsAndWeights(t *testing.T) {
	encoded, err := parseTSVector(t.Context(), `'z':3 'a':2A,1B,2C`, 4096)
	if err != nil {
		t.Fatal(err)
	}
	text, err := formatTSVector(t.Context(), encoded, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if text != `'a':1B,2A 'z':3` {
		t.Fatalf("normalized vector = %q", text)
	}
}

func TestFullTextWeightedPhraseMatch(t *testing.T) {
	vector, err := parseTSVector(t.Context(), `'alpha':1A 'beta':2B,8D`, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		query string
		want  bool
	}{
		{`'alpha':A <-> 'beta':B`, true},
		{`'alpha':B <-> 'beta':B`, false},
		{`'alpha':A <2> 'beta':B`, false},
		{`'alpha':A & !'gamma'`, true},
	} {
		query, err := parseTSQuery(t.Context(), scenario.query, 4096)
		if err != nil {
			t.Fatal(err)
		}
		got, err := fullTextMatch(t.Context(), model.BytesVal(vector), model.BytesVal(query))
		if err != nil {
			t.Fatal(err)
		}
		if (got.IntVal != 0) != scenario.want {
			t.Errorf("match %q = %t, want %t", scenario.query, got.IntVal != 0, scenario.want)
		}
	}
}

func TestNativeSnowballEnglishStemmingAndStopWords(t *testing.T) {
	stemmer := NativeTextSearchStemmer{}
	for _, scenario := range []struct{ input, want string }{{"running", "run"}, {"ponies", "poni"}} {
		got, err := stemmer.StemTextSearch(t.Context(), "english", scenario.input)
		if err != nil {
			t.Fatal(err)
		}
		if got != scenario.want {
			t.Errorf("stem %q = %q, want %q", scenario.input, got, scenario.want)
		}
	}
	stop, err := stemmer.IsTextSearchStopWord(t.Context(), "english", "the")
	if err != nil {
		t.Fatal(err)
	}
	if !stop {
		t.Fatal("English stop-word list omitted the")
	}
}

func TestFullTextBinaryRoundTrip(t *testing.T) {
	for _, scenario := range []struct {
		parse  func(context.Context, string, int64) ([]byte, error)
		format func(context.Context, []byte, int64) (string, error)
		input  string
		receive model.FunctionID
	}{
		{parseTSVector, formatTSVector, `'a':1A,7D 'b'`, 3639},
		{parseTSQuery, formatTSQuery, `!'a':AB* | ('b' <3> 'c')`, 3641},
	} {
		first, err := scenario.parse(t.Context(), scenario.input, 4096)
		if err != nil {
			t.Fatal(err)
		}
		sent, err := fullTextSend(model.BytesVal(first), CallBudget{OutputBytes: 4096})
		if err != nil {
			t.Fatal(err)
		}
		received, err := fullTextReceive(t.Context(), scenario.receive, sent, CallBudget{OutputBytes: 4096})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, received.BytesVal) {
			t.Fatal("binary send/receive changed the value")
		}
		text, err := scenario.format(t.Context(), first, 4096)
		if err != nil {
			t.Fatal(err)
		}
		second, err := scenario.parse(t.Context(), text, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("binary value changed through %q", text)
		}
	}
}

func TestFullTextJSONVectorSelectsRequestedScalars(t *testing.T) {
	configuration, _ := builtinTextSearchConfiguration(tsSimpleConfigOID)
	flags, err := parseTSJSONFlags(t.Context(), `["string", "key"]`)
	if err != nil {
		t.Fatal(err)
	}
	vector, err := jsonTSVector(t.Context(), configuration, `{"title":"Red Fox","count":4}`, flags, 4096)
	if err != nil {
		t.Fatal(err)
	}
	text, err := formatTSVector(t.Context(), vector, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, lexeme := range []string{"'count'", "'fox'", "'red'", "'title'"} {
		if !strings.Contains(text, lexeme) {
			t.Errorf("JSON vector %q lacks %s", text, lexeme)
		}
	}
	if strings.Contains(text, "'4'") {
		t.Fatalf("JSON vector %q included an unrequested number", text)
	}
}

func TestFullTextRejectsMalformedTextAndBinary(t *testing.T) {
	if _, err := parseTSVector(t.Context(), `'a':0`, 4096); err == nil {
		t.Fatal("zero tsvector position was accepted")
	}
	if _, err := parseTSQuery(t.Context(), `'a' &`, 4096); err == nil {
		t.Fatal("dangling tsquery operator was accepted")
	}
	if _, err := decodeTSVector(t.Context(), []byte{0, 0, 0, 1, 'a'}); err == nil {
		t.Fatal("truncated tsvector binary was accepted")
	}
	if _, _, err := decodeTSQuery(t.Context(), []byte{0, 0, 0, 1, 1}); err == nil {
		t.Fatal("truncated tsquery binary was accepted")
	}
}

func TestFullTextCancellationAndOutputBudgets(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := parseTSVector(ctx, `'`+strings.Repeat("a", 1000)+`'`, 4096); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled parse = %v", err)
	}
	if _, err := parseTSVector(t.Context(), `'alphabet'`, 4); err == nil {
		t.Fatal("short vector budget was accepted")
	} else {
		requireReplacementSQLState(t, err, "54000")
	}
	if _, err := parseTSQuery(t.Context(), `'alphabet'`, 4); err == nil {
		t.Fatal("short query budget was accepted")
	} else {
		requireReplacementSQLState(t, err, "54000")
	}
}
