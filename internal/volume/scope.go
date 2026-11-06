package volume

import "regexp"

// SharedScope is the scope of volumes any tenant may attach. Every other scope
// is the ID of the tenant that owns the volume.
const SharedScope = "shared"

var (
	// Tenant IDs are canonical UUIDs (uuid.New().String() in the tenant handlers).
	tenantIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	digestPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidScope reports whether scope is SharedScope or a tenant ID. A valid scope
// is safe as a path segment.
func ValidScope(scope string) bool {
	return scope == SharedScope || tenantIDPattern.MatchString(scope)
}

// ValidDigest reports whether digest is a sha256 in lowercase hex.
func ValidDigest(digest string) bool {
	return digestPattern.MatchString(digest)
}
