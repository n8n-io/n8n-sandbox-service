package netpolicy

// PrivateRangesV4 lists IPv4 destinations blocked for sandbox egress. Matches
// Docker runner netrules and Firecracker per-netns FORWARD policy.
var PrivateRangesV4 = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"127.0.0.0/8",
	"100.64.0.0/10",
	"198.18.0.0/15",
	"240.0.0.0/4",
}
