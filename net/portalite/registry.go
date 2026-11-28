package portalite

// defaultRelays is the built-in relay bootstrap set. It mirrors the union of
// relays currently published by the network's /discovery endpoint, validated
// against the tunnel protocol this SDK speaks. Relay availability is
// operational state, not a static guarantee.
var defaultRelays = [...]string{
	"https://s-h.day",
	"https://portal.rabbitson87.dev",
	"https://portal.dawnfullstack.com",
}

// DefaultRelays returns the canonical built-in relay URLs in registry order.
// Each call returns an independent slice.
func DefaultRelays() []string {
	result := make([]string, len(defaultRelays))
	copy(result, defaultRelays[:])
	return result
}
