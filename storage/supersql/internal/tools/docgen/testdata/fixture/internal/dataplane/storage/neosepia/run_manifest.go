package neosepia

import "fmt"

const (
	runManifestMagic       = "NSRUNS01"
	runManifestHeaderBytes = 160
	runManifestEntryBytes  = 80
)

func runManifestName(position uint64) string { return fmt.Sprintf("%020d.runs", position) }
func preparedRunName(position uint64, index int) string {
	return fmt.Sprintf("%020d-%08x.sst", position, index)
}
