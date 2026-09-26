package ike

import "strings"

func dataAuthPayloads(input []payload, protocol string) []payload {
	attributes := []uint16{configApplicationVersion}
	if !strings.EqualFold(protocol, "IPV6") {
		attributes = append(attributes, configInternalIPv4Address, configInternalIPv4DNS)
	}
	if !strings.EqualFold(protocol, "IP") {
		attributes = append(attributes, configInternalIPv6Address, configInternalIPv6DNS)
	}
	output := make([]payload, 0, len(input))
	for _, p := range input {
		// A second PDN must not tell the peer to delete the existing IMS IKE SA.
		if p.Type == payloadNotify {
			kind, _, err := parseNotify(p)
			if err == nil && kind == notifyInitialContact {
				continue
			}
		}
		if p.Type == payloadCP {
			p = configurationAttributes(attributes)
		}
		output = append(output, p)
	}
	return output
}
