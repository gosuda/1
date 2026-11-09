package main

import (
	"fmt"
	"go/ast"
	"regexp"
	"sort"
	"strings"
)

// storageConstant is one allowlisted on-disk format constant: where it is
// declared, so the tool can read the literal from source instead of restating it.
type storageConstant struct {
	file string
	name string
}

// storageConstants is the allowlist of format constants that appear in
// storage-format.md. Renaming or moving one fails the document build loudly
// instead of silently dropping the row.
var storageConstants = []storageConstant{
	{"internal/dataplane/storage/neosepia/pax_format.go", "paxMagic"},
	{"internal/dataplane/storage/neosepia/pax_format.go", "paxVersion"},
	{"internal/dataplane/storage/neosepia/pax_format.go", "paxHeaderSize"},
	{"internal/dataplane/storage/neosepia/pax_format.go", "paxVersionSize"},
	{"internal/dataplane/storage/neosepia/backend_state.go", "databaseStateMagic"},
	{"internal/dataplane/storage/neosepia/backend_state.go", "databaseStateName"},
	{"internal/dataplane/storage/neosepia/backend_state.go", "databaseStatePending"},
	{"internal/dataplane/storage/neosepia/backend_state.go", "databaseStateHeader"},
	{"internal/dataplane/storage/neosepia/commit_store.go", "commitMagic"},
	{"internal/dataplane/storage/neosepia/commit_store.go", "commitEndMagic"},
	{"internal/dataplane/storage/neosepia/commit_store.go", "commitHeaderSize"},
	{"internal/dataplane/storage/neosepia/run_manifest.go", "runManifestMagic"},
	{"internal/dataplane/storage/neosepia/run_manifest.go", "runManifestHeaderBytes"},
	{"internal/dataplane/storage/neosepia/run_manifest.go", "runManifestEntryBytes"},
	{"internal/dataplane/storage/neosepia/base_manifest.go", "baseManifestMagic"},
	{"internal/dataplane/storage/neosepia/base_manifest.go", "baseManifestHeader"},
	{"internal/dataplane/storage/neosepia/base_manifest.go", "baseManifestEntryBytes"},
	{"internal/dataplane/storage/neosepia/base_manifest.go", "baseTransactionBytes"},
	{"internal/dataplane/storage/neosepia/base_manifest.go", "baseManifestPending"},
	{"internal/dataplane/storage/neosepia/sst_write.go", "sstMagic"},
	{"internal/dataplane/storage/neosepia/sst_write.go", "sstHeaderSize"},
	{"internal/dataplane/storage/neosepia/sst_write.go", "sstIndexHeaderSize"},
	{"internal/dataplane/storage/neosepia/backend_checkpoint.go", "checkpointMagic"},
	{"internal/dataplane/storage/neosepia/backend_checkpoint.go", "checkpointPrefix"},
	{"internal/controlplane/schema/jobs.go", "jobEncoding"},
	{"open.go", "nativeSpoolDirectory"},
	{"open.go", "nativeFenceSecretFile"},
	{"open.go", "nativeFenceSecretSize"},
}

// storageNameFunctions are the file-name constructors whose format string is
// part of the on-disk layout.
var storageNameFunctions = []storageConstant{
	{"internal/dataplane/storage/neosepia/commit_store.go", "commitName"},
	{"internal/dataplane/storage/neosepia/commit_store.go", "pendingCommitName"},
	{"internal/dataplane/storage/neosepia/run_manifest.go", "runManifestName"},
	{"internal/dataplane/storage/neosepia/run_manifest.go", "preparedRunName"},
	{"internal/dataplane/storage/neosepia/base_manifest.go", "baseManifestName"},
}

// legacyMagicPattern matches the retired format magics still read by the
// recovery path ("NSBASE02", "NSSST002").
var legacyMagicPattern = regexp.MustCompile(`^NS[A-Z]+[0-9]{2,}$`)

func renderStorageFormat(g *generator) ([]byte, error) {
	writer := newMarkdownWriter()
	writer.heading(1, "Storage format")
	writer.paragraph("The on-disk format of one native database directory, as the storage layer reads and writes it. Every constant below is read from its declaration, so this document cannot drift from the code without failing the build.")

	writer.heading(2, "Directory layout")
	writer.paragraph("A database directory contains:")
	writer.table([]string{"Path", "Kind", "Description"}, [][]string{
		{"`database.state`", "file", "Published database state: format magic, generation, committed log position, commit and closed timestamps, history floor, fence, and the clock scale versions. Rewritten atomically through `database.state.pending`."},
		{"`database.state.pending`", "file", "Staging name for the state rewrite; discarded on recovery."},
		{"`fence.secret`", "file", "Fence authority secret, created on first open. A directory without it fails fence verification and is not a native database."},
		{"`tmp/`", "directory", "Spill directory for preparation buffers."},
		{"`commits/`", "directory", "Published commit segments, one per log position."},
		{"`commits/<020d>.wal`", "file", "One immutable commit segment: BEGIN frame, length-framed mutations, COMMIT frame."},
		{"`commits/.<020d>.pending`", "file", "Staging name for a commit segment; discarded on recovery."},
		{"`runs/`", "directory", "Run manifests, run files and base manifests."},
		{"`runs/<020d>.runs`", "file", "Run manifest for one log position: the prepared run files a commit published."},
		{"`runs/<020d>-<8hex>.sst`", "file", "One PAX run file. `<8hex>` is the zero-padded index of the file within its position."},
		{"`runs/base-<020d>.runs`", "file", "Base manifest of one maintenance generation, retiring the commits and run manifests it covers."},
		{"`runs/.base.pending`", "file", "Staging name for the base manifest; discarded on recovery."},
		{"`.lock`", "file", "Directory lock held for the lifetime of an open backend."},
		{"`.checkpoint-<hex>/`", "directory", "Published checkpoint directory, linked into a hot backup."},
	})

	writer.heading(2, "Format constants")
	constants, err := collectStorageConstants(g)
	if err != nil {
		return nil, err
	}
	writer.table([]string{"Constant", "Value", "Location"}, constants)

	writer.heading(2, "File name patterns")
	patterns, err := collectNamePatterns(g)
	if err != nil {
		return nil, err
	}
	writer.table([]string{"Constructor", "Format", "Location"}, patterns)

	writer.heading(2, "Legacy format markers")
	legacy, err := collectLegacyMagics(g)
	if err != nil {
		return nil, err
	}
	writer.paragraph(plural(len(legacy), "retired magic value is still recognised so that an older directory is rejected or read rather than silently misread.", "retired magic values are still recognised so that an older directory is rejected or read rather than silently misread."))
	writer.table([]string{"Magic", "Location"}, legacy)

	writer.heading(2, "Recovery order")
	writer.paragraph("`neosepia.Open` recovers a directory in this order and never writes to a directory it has not validated:")
	writer.line("1. Inspect the directory: a regular `database.state` whose magic is valid is a native database; any other layout is reported as an incompatible format, which the root package maps to `*UpgradeRequired` when no `database.state` exists.")
	writer.line("2. Open the directory and acquire `.lock`.")
	writer.line("3. Read `database.state`; when absent, write the initial generation from the supplied clock scale.")
	writer.line("4. Verify the persisted read fence against the fence authority when a closed timestamp exists.")
	writer.line("5. `recoverPublishedRuns`: load the base manifest when a base generation is set, open its run files, then replay published commit segments.")
	writer.line("6. `recoverCommits`: scan `commits/` in position order, require contiguous positions and monotonic commit timestamps, and discard `.pending` segments; a reused transaction identity is corruption.")
	writer.line("7. `validateState`: cross-check the recovered state against the persisted state, then publish the run set and frontier.")
	writer.line("8. Remove `database.state.pending`, sync the commit directory, drop superseded run files, retire recovered WAL segments, and clean up checkpoint directories.")
	writer.blank()
	writer.paragraph("A failure in any step is fail-stop: the backend returns the error (`storage.ErrCorruptStorage` for a violated format or ordering invariant) and closes itself rather than serving a partially recovered database.")

	writer.heading(2, "Legacy-format gate")
	writer.paragraph("`neosepia.Open` refuses a directory whose layout is not native (`ErrIncompatibleFormat`) and never writes to it. The root package maps that verdict to `*UpgradeRequired` when the directory has no `database.state`, so a legacy directory requires the offline copy-upgrade instead of being reopened in place.")

	g.note("storage-format: %d constants, %d name patterns, %d legacy magics", len(constants), len(patterns), len(legacy))
	return writer.bytes(), nil
}

// collectStorageConstants resolves the allowlisted constants from their files.
func collectStorageConstants(g *generator) ([][]string, error) {
	byFile := map[string][]constSpec{}
	rows := make([][]string, 0, len(storageConstants))
	for _, entry := range storageConstants {
		constants, ok := byFile[entry.file]
		if !ok {
			file, err := g.parse(entry.file)
			if err != nil {
				return nil, err
			}
			constants = g.fileConstants(entry.file, file)
			byFile[entry.file] = constants
		}
		constant, found := findConst(constants, entry.name)
		if !found {
			return nil, fmt.Errorf("%s: constant %s not found", entry.file, entry.name)
		}
		rows = append(rows, []string{entry.name, constant.value, g.position(constant.position)})
	}
	return rows, nil
}

// collectNamePatterns renders the format string of each file-name constructor.
func collectNamePatterns(g *generator) ([][]string, error) {
	rows := make([][]string, 0, len(storageNameFunctions))
	for _, entry := range storageNameFunctions {
		file, err := g.parse(entry.file)
		if err != nil {
			return nil, err
		}
		body, found := findFuncBody(file, entry.name)
		if !found {
			return nil, fmt.Errorf("%s: function %s not found", entry.file, entry.name)
		}
		format := ""
		ast.Inspect(body, func(node ast.Node) bool {
			if format != "" {
				return false
			}
			call, ok := node.(*ast.CallExpr)
			if !ok || callName(call) != "Sprintf" || len(call.Args) == 0 {
				return true
			}
			if value, ok := stringLiteral(call.Args[0]); ok {
				format = value
			}
			return true
		})
		if format == "" {
			return nil, fmt.Errorf("%s: %s has no Sprintf format literal", entry.file, entry.name)
		}
		rows = append(rows, []string{entry.name, "`" + format + "`", g.position(body.Pos())})
	}
	return rows, nil
}

// collectLegacyMagics finds the retired magic literals still handled by the
// readers: every NS-prefixed literal that is not a current magic.
func collectLegacyMagics(g *generator) ([][]string, error) {
	current := map[string]bool{}
	for _, entry := range storageConstants {
		if !strings.HasSuffix(entry.name, "Magic") {
			continue
		}
		file, err := g.parse(entry.file)
		if err != nil {
			return nil, err
		}
		if constant, ok := findConst(g.fileConstants(entry.file, file), entry.name); ok {
			if value, ok := unquote(constant.value); ok {
				current[value] = true
			}
		}
	}
	seen := map[string]string{}
	err := g.walkSources(func(rel string, file *ast.File) error {
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok {
				return true
			}
			value, ok := stringLiteral(literal)
			if !ok || !legacyMagicPattern.MatchString(value) || current[value] {
				return true
			}
			if _, exists := seen[value]; !exists {
				seen[value] = g.position(literal.Pos())
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	rows := make([][]string, 0, len(seen))
	for _, magic := range sortedKeys(seen) {
		rows = append(rows, []string{"`" + magic + "`", seen[magic]})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i][1] < rows[j][1] })
	return rows, nil
}
