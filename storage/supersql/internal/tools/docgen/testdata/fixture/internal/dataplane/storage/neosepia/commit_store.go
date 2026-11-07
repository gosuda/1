package neosepia

import "fmt"

const commitHeaderSize = 128
const commitMagic = "NSTXBEG2"
const commitEndMagic = "NSTXCMT2"

func commitName(position uint64) string        { return fmt.Sprintf("%020d.wal", position) }
func pendingCommitName(position uint64) string { return fmt.Sprintf(".%020d.pending", position) }
