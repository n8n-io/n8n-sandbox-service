package manager

// NetworkPolicy defines per-sandbox IP-based allow/deny lists.
type NetworkPolicy struct {
	// AllowedIPs are CIDRs permitted even if they fall in private ranges.
	AllowedIPs []string `json:"allowed_ips,omitempty"`
	// DeniedIPs are CIDRs blocked even if they are public.
	DeniedIPs []string `json:"denied_ips,omitempty"`
}
