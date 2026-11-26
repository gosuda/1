package neosepia

import "fmt"

const (
	baseManifestMagic      = "NSBASE03"
	baseManifestHeader     = 176
	baseManifestEntryBytes = 144
	baseTransactionBytes   = 80
	baseManifestPending    = ".base.pending"
)

func baseManifestName(generation uint64) string {
	return fmt.Sprintf("base-%020d.runs", generation)
}

func decodeBaseManifest(magic string) bool {
	switch magic {
	case baseManifestMagic, "NSBASE02":
		return true
	}
	return false
}
