package neosepia

func decodeSSTHeader(magic string) bool {
	switch magic {
	case sstMagic, "NSSST002":
		return true
	}
	return false
}
