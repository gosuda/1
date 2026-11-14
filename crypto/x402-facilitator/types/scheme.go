package types

type Scheme string

const (
	// Exact is the x402 v2 "exact amount" payment scheme. It is currently
	// the only scheme this facilitator supports; other schemes (e.g. the
	// forthcoming "upto") can be added alongside it when they land.
	Exact Scheme = "exact"
)

type X402Version int

const (
	X402VersionV1 X402Version = 1
	X402VersionV2 X402Version = 2
)

type Signer func(digest []byte) (signature []byte, err error)
