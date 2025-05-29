package drivers

import (
	"encoding/json"
	"fmt"
	"net"
)

// FeatureOpts specify how firewall features are setup.
type FeatureOpts struct {
	ICMPDHCPDNSAccess bool // Add rules to allow ICMP, DHCP and DNS access.
	ForwardingAllow   bool // Add rules to allow IP forwarding. Blocked if false.
}

// SNATOpts specify how SNAT rules are setup.
type SNATOpts struct {
	Append      bool       // Append rules (has no effect if driver doesn't support it).
	Subnet      *net.IPNet // Subnet of source network used to identify candidate traffic.
	SNATAddress net.IP     // SNAT IP address to use. If nil then MASQUERADE is used.
}

// Opts for setting up the firewall.
type Opts struct {
	FeaturesV4 *FeatureOpts // Enable IPv4 firewall with specified options. Off if not provided.
	FeaturesV6 *FeatureOpts // Enable IPv6 firewall with specified options. Off if not provided.
	SNATV4     *SNATOpts    // Enable IPv4 SNAT with specified options. Off if not provided.
	SNATV6     *SNATOpts    // Enable IPv6 SNAT with specified options. Off if not provided.
	ACL        bool         // Enable ACL during setup.
	AddressSet bool         // Enable address sets, only for netfilter.
}

// ACLRule represents an ACL rule that can be added to a firewall.
type ACLRule struct {
	Direction       string // Either "ingress" or "egress.
	Action          string
	Log             bool   // Whether or not to log matched packets.
	LogName         string // Log label name (requires Log be true).
	Source          string
	Destination     string
	Protocol        string
	SourcePort      string
	DestinationPort string
	ICMPType        string
	ICMPCode        string
}

// AddressForward represents a NAT address forward.
type AddressForward struct {
	ListenAddress net.IP
	TargetAddress net.IP
	Protocol      string
	ListenPorts   []uint64
	TargetPorts   []uint64
	SNAT          bool
}

// AddressSet represent an address set.
type AddressSet struct {
	Name      string
	Addresses []string
}

// NftListSetsOutput structure to read JSON output of set listing.
type NftListSetsOutput struct {
	Nftables []NftListSetsEntry `json:"nftables"`
}

// NftListSetsEntry structure to read JSON output of nft set listing.
type NftListSetsEntry struct {
	Metainfo *NftMetainfo `json:"metainfo,omitempty"`
	Set      *NftSet      `json:"set,omitempty"`
}

// NftMetainfo structure representing metainformation returned by nft.
type NftMetainfo struct {
	Version           string `json:"version"`
	ReleaseName       string `json:"release_name"`
	JSONSchemaVersion int    `json:"json_schema_version"`
}

// NftSet structure to parse the JSON of a set returned by nft -j list sets.
type NftSet struct {
	Family string    `json:"family"`
	Name   string    `json:"name"`
	Table  string    `json:"table"`
	Type   string    `json:"type"`
	Handle int       `json:"handle"`
	Flags  []string  `json:"flags"`
	Elem   ElemField `json:"elem"`
}

// ElemField supports both string elements (IP, MAC) and dictionary-based CIDR elements.
// In order to parse it correctly a custom unsmarshalling is defined in drivers_nftables.go .
type ElemField struct {
	Addresses []string // Stores plain addresses and CIDR notations as strings.
}

// UnmarshalJSON handles both plain strings and CIDR dictionaries inside `elem`.
func (e *ElemField) UnmarshalJSON(data []byte) error {
	var rawElems []any
	err := json.Unmarshal(data, &rawElems)
	if err != nil {
		return err
	}

	for _, elem := range rawElems {
		switch v := elem.(type) {
		case string:
			// Plain address (IPv4, IPv6, or MAC).
			e.Addresses = append(e.Addresses, v)
		case map[string]any:
			// CIDR notation (prefix dictionary).
			prefix, ok := v["prefix"].(map[string]any)
			if ok {
				addr, addrOk := prefix["addr"].(string)
				lenFloat, lenOk := prefix["len"].(float64) // JSON numbers are float64 by default.
				if addrOk && lenOk {
					e.Addresses = append(e.Addresses, fmt.Sprintf("%s/%d", addr, int(lenFloat)))
				}
			}

		default:
			return fmt.Errorf("Unsupported element type in NFTables set: %v", elem)
		}
	}

	return nil
}
