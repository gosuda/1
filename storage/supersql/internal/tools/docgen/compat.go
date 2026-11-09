package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"io"
	"os"
	"sort"
	"strings"
)

// The manifest shapes below mirror internal/query/bind/internal/genfeatures,
// which is the only other reader of testdata/postgres/embedded_features.json.
// They are decoded with DisallowUnknownFields so a manifest change fails the
// document build instead of silently rendering a partial table.
type featureManifest struct {
	Profile           string            `json:"profile"`
	PostgreSQLVersion int               `json:"postgres_version_num"`
	Source            map[string]string `json:"source"`
	Features          []manifestFeature `json:"features"`
}

type manifestFeature struct {
	ID          string            `json:"id"`
	Disposition string            `json:"disposition"`
	Reason      string            `json:"reason"`
	ASTMatch    []string          `json:"ast_match"`
	Objects     []manifestObject  `json:"resolved_objects"`
	SQLState    string            `json:"sqlstate"`
	Statements  []json.RawMessage `json:"regression_statements"`
}

type manifestObject struct {
	Kind          string   `json:"kind"`
	Namespace     string   `json:"namespace"`
	Name          string   `json:"name"`
	OID           uint32   `json:"oid"`
	Arguments     []uint32 `json:"argument_types"`
	ArgumentNames []string `json:"argument_type_names"`
	Result        uint32   `json:"result_type"`
	ResultName    string   `json:"result_type_name"`
	Strict        bool     `json:"strict"`
	Volatility    string   `json:"volatility"`
	FunctionKind  string   `json:"function_kind"`
	ReturnsSet    bool     `json:"returns_set"`
	Variadic      uint32   `json:"variadic_type"`
	Mutable       bool     `json:"mutable"`
}

// errInvalidManifest reports a manifest the documenter refuses to describe.
var errInvalidManifest = errors.New("invalid embedded feature manifest")

const (
	// expectedProfile and expectedVersionNum pin the documented profile to the
	// PostgreSQL release the manifest was generated from.
	expectedProfile    = "embedded_pg18"
	expectedVersionNum = 180004
	// expectedFeatureCount is the fixed exclusion set of the embedded profile.
	expectedFeatureCount = 25
	// unsupportedSQLState is the SQLSTATE of every profile exclusion.
	unsupportedSQLState = "0A000"
)

// rejection is one SQLSTATE 0A000 site outside the profile table.
type rejection struct {
	location string
	message  string
}

func renderCompatibility(g *generator) ([]byte, error) {
	manifest, err := loadFeatureManifest(g)
	if err != nil {
		return nil, err
	}
	rejections, err := collectFeatureRejections(g)
	if err != nil {
		return nil, err
	}
	writer := newMarkdownWriter()
	writer.heading(1, "SQL compatibility")
	writer.paragraph(fmt.Sprintf(
		"The embedded profile is `%s` against PostgreSQL %s (`postgres_version_num` %d). This document is generated from `testdata/postgres/embedded_features.json`, which lists the fixed feature exclusions of the profile and the catalog objects each exclusion resolves.",
		manifest.Profile, postgresVersion(manifest.PostgreSQLVersion), manifest.PostgreSQLVersion))
	writer.heading(2, "Profile source")
	writer.table([]string{"Key", "Value"}, [][]string{
		{"profile", manifest.Profile},
		{"postgres_version_num", fmt.Sprint(manifest.PostgreSQLVersion)},
		{"archive", manifest.Source["archive"]},
		{"archive_sha256", manifest.Source["sha256"]},
		{"catalog", manifest.Source["catalog"]},
		{"regression", manifest.Source["regression"]},
		{"auxiliary_regression_revision", manifest.Source["auxiliary_regression_revision"]},
		{"features", fmt.Sprint(len(manifest.Features))},
	})
	writer.heading(2, "Feature dispositions")
	rows := make([][]string, 0, len(manifest.Features))
	for _, feature := range manifest.Features {
		rows = append(rows, []string{
			feature.ID,
			feature.Disposition,
			feature.SQLState,
			fmt.Sprint(len(feature.ASTMatch)),
			fmt.Sprint(len(feature.Objects)),
			feature.Reason,
		})
	}
	writer.table([]string{"Feature", "Disposition", "SQLSTATE", "AST match", "Objects", "Reason"}, rows)
	writer.heading(2, "Feature details")
	for _, feature := range manifest.Features {
		writer.heading(3, "`"+feature.ID+"`")
		writer.paragraph(feature.Reason)
		writer.table([]string{"Field", "Value"}, [][]string{
			{"disposition", feature.Disposition},
			{"sqlstate", feature.SQLState},
			{"regression_statements", fmt.Sprint(len(feature.Statements))},
		})
		if len(feature.ASTMatch) > 0 {
			writer.paragraph("AST match strings:")
			for _, match := range feature.ASTMatch {
				writer.line("- `%s`", strings.ReplaceAll(match, "`", "'"))
			}
			writer.blank()
		}
		writeObjectCounts(writer, feature)
	}
	writeRejections(writer, rejections)
	g.note("sql-compatibility: %d features, %d additional 0A000 rejection sites", len(manifest.Features), len(rejections))
	return writer.bytes(), nil
}

// postgresVersion renders a server_version_num as major.minor.
func postgresVersion(versionNum int) string {
	return fmt.Sprintf("%d.%d", versionNum/10000, versionNum%10000)
}

// writeObjectCounts renders the resolved-object histogram of one feature.
func writeObjectCounts(writer *mdWriter, feature manifestFeature) {
	if len(feature.Objects) == 0 {
		writer.paragraph("No resolved catalog objects.")
		return
	}
	counts := map[string]int{}
	namespaces := map[string]int{}
	for _, object := range feature.Objects {
		counts[object.Kind]++
		namespaces[object.Kind+"@"+object.Namespace]++
	}
	writer.paragraph(fmt.Sprintf("Resolved objects: %d.", len(feature.Objects)))
	rows := make([][]string, 0, len(counts))
	for _, kind := range sortedKeys(counts) {
		rows = append(rows, []string{kind, fmt.Sprint(counts[kind])})
	}
	writer.table([]string{"Kind", "Count"}, rows)
	rows = rows[:0]
	for _, key := range sortedKeys(namespaces) {
		kind, namespace, _ := strings.Cut(key, "@")
		rows = append(rows, []string{kind, namespace, fmt.Sprint(namespaces[key])})
	}
	writer.table([]string{"Kind", "Namespace", "Count"}, rows)
}

// writeRejections lists 0A000 sites that the profile table does not cover.
func writeRejections(writer *mdWriter, rejections []rejection) {
	writer.heading(2, "Additional 0A000 rejections outside the profile table")
	writer.paragraph(plural(len(rejections), "call site in non-test code rejects SQL with SQLSTATE `0A000`; the profile table above documents the feature classes, this list names every literal site.", "call sites in non-test code reject SQL with SQLSTATE `0A000`; the profile table above documents the feature classes, this list names every literal site."))
	if len(rejections) == 0 {
		writer.paragraph("No additional rejection site.")
		return
	}
	rows := make([][]string, 0, len(rejections))
	for _, site := range rejections {
		rows = append(rows, []string{site.location, "`" + site.message + "`"})
	}
	writer.table([]string{"Location", "Message"}, rows)
}

// loadFeatureManifest decodes the embedded feature manifest, refusing unknown
// fields and a profile that is not the pinned embedded profile.
func loadFeatureManifest(g *generator) (featureManifest, error) {
	path := "testdata/postgres/embedded_features.json"
	data, err := os.ReadFile(g.path(path))
	if err != nil {
		return featureManifest{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var manifest featureManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return featureManifest{}, fmt.Errorf("%w: %s: %v", errInvalidManifest, path, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return featureManifest{}, fmt.Errorf("%w: trailing JSON content in %s", errInvalidManifest, path)
	}
	if manifest.Profile != expectedProfile || manifest.PostgreSQLVersion != expectedVersionNum {
		return featureManifest{}, fmt.Errorf("%w: profile %q version %d", errInvalidManifest, manifest.Profile, manifest.PostgreSQLVersion)
	}
	if len(manifest.Features) != expectedFeatureCount {
		return featureManifest{}, fmt.Errorf("%w: %d features, want %d", errInvalidManifest, len(manifest.Features), expectedFeatureCount)
	}
	return manifest, nil
}

// collectFeatureRejections scans every non-test Go file for the literal
// SQLSTATE 0A000 rejection sites: bindError("0A000", ...) calls and
// Code: "0A000" composite-literal fields.
func collectFeatureRejections(g *generator) ([]rejection, error) {
	var sites []rejection
	err := g.walkSources(func(rel string, file *ast.File) error {
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && callName(call) == "bindError" && len(call.Args) > 1 {
				if code, ok := stringLiteral(call.Args[0]); ok && code == unsupportedSQLState {
					message, _ := stringLiteral(call.Args[1])
					sites = append(sites, rejection{location: g.position(call.Pos()), message: message})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = g.walkSources(func(rel string, file *ast.File) error {
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, element := range literal.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := field.Key.(*ast.Ident)
				if !ok || key.Name != "Code" {
					continue
				}
				if code, ok := stringLiteral(field.Value); ok && code == unsupportedSQLState {
					sites = append(sites, rejection{location: g.position(field.Pos()), message: "model.Error{Code: \"0A000\"}"})
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].location != sites[j].location {
			return sites[i].location < sites[j].location
		}
		return sites[i].message < sites[j].message
	})
	return sites, nil
}
