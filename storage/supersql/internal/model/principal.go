package model

import "strings"

const (
	defaultPrincipalName = "embedded"
	defaultPrincipalOID  = OID(10)
)

// Principal is the immutable logical owner, not an authorization capability.
type Principal struct {
	name      string
	oid       OID
	superuser bool
	explicit  bool
}

// NewPrincipal constructs a Principal for name and oid.
// An empty name (or "none") defaults to the built-in "embedded" superuser;
// a zero oid defaults to the built-in principal OID (10).
func NewPrincipal(name string, oid OID) Principal {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if normalized == "" || normalized == "none" {
		return Principal{oid: oid}
	}
	isDefaultSuper := normalized == defaultPrincipalName || normalized == "supersql" || normalized == "postgres"
	return Principal{
		name:      normalized,
		oid:       oid,
		superuser: isDefaultSuper,
		explicit:  true,
	}
}

func (p Principal) Name() string {
	if p.name == "" {
		return defaultPrincipalName
	}
	return p.name
}

func (p Principal) OID() OID {
	if p.oid == 0 {
		return defaultPrincipalOID
	}
	return p.oid
}

// IsSuperuser reports whether p represents the built-in default superuser
// identity. Catalog-defined roles must be checked against catalog RoleMetadata
// for explicit SUPERUSER privileges.
func (p Principal) IsSuperuser() bool {
	if !p.explicit {
		return true
	}
	return p.superuser
}
