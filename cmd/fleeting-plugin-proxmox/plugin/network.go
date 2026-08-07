package plugin

import (
	"errors"
	"net"

	"github.com/luthermonson/go-proxmox"
)

var (
	ErrNoIPAddress       = errors.New("failed to determine IP address for instance")
	ErrTooManyPotentials = errors.New("cannot determine IP address when instance has greater than 1 non-loopback interface and no interface name was specified. Configure 'instance_network_interface' in your plugin config.")
)

// Determines internal and external address for given interfaces.
func determineAddresses(networkInterfaces []*proxmox.AgentNetworkIface, requestedInterface string, requestedProtocol NetworkProtocol) (string, string, error) {
	// Filter out any interfaces without a valid hardware address
	var potentialInterfaces []*proxmox.AgentNetworkIface
	for _, networkInterface := range networkInterfaces {
		// Loopback hardware address is all zeros (*nix) or empty (Windows)
		if networkInterface.HardwareAddress != "00:00:00:00:00:00" && networkInterface.HardwareAddress != "" {
			potentialInterfaces = append(potentialInterfaces, networkInterface)
		}
	}

	if requestedInterface == "" && len(potentialInterfaces) > 1 {
		return "", "", ErrTooManyPotentials
	}

	internalIPv4, externalIPv4, internalIPv6, externalIPv6 := determinePossibleAddresses(potentialInterfaces, requestedInterface)

	// IPv6 (or Any)
	if requestedProtocol == NetworkProtocolIPv6 || requestedProtocol == NetworkProtocolAny {
		// External address is required so use internal if needed
		if externalIPv6 == "" {
			externalIPv6 = internalIPv6
		}

		if externalIPv6 != "" {
			return internalIPv6, externalIPv6, nil
		}
	}

	// IPv4 (or Any)
	if requestedProtocol == NetworkProtocolIPv4 || requestedProtocol == NetworkProtocolAny {
		// External address is required so use internal if needed
		if externalIPv4 == "" {
			externalIPv4 = internalIPv4
		}

		if externalIPv4 != "" {
			return internalIPv4, externalIPv4, nil
		}
	}

	// Did not find the address in requested protocol
	return "", "", ErrNoIPAddress
}

// Finds possible IPv4 and IPv6 addresses for given interfaces.
func determinePossibleAddresses(networkInterfaces []*proxmox.AgentNetworkIface, requestedInterface string) (string, string, string, string) {
	internalIPv4 := ""
	externalIPv4 := ""
	internalIPv6 := ""
	externalIPv6 := ""

	for _, networkInterface := range networkInterfaces {
		if requestedInterface != "" && networkInterface.Name != requestedInterface {
			continue
		}

		for _, address := range networkInterface.IPAddresses {
			parsedAddress := net.ParseIP(address.IPAddress)

			if parsedAddress == nil {
				continue
			}

			if parsedAddress.IsLoopback() || parsedAddress.IsUnspecified() {
				continue
			}

			if address.IPAddressType == NetworkProtocolIPv4 {
				if parsedAddress.IsPrivate() {
					internalIPv4 = address.IPAddress
				} else if parsedAddress.IsGlobalUnicast() {
					externalIPv4 = address.IPAddress
				}
			}

			if address.IPAddressType == NetworkProtocolIPv6 {
				if parsedAddress.IsPrivate() {
					internalIPv6 = address.IPAddress
				} else if parsedAddress.IsGlobalUnicast() {
					externalIPv6 = address.IPAddress
				}
			}
		}

		// We found requested interface so we can break the loop
		break
	}

	return internalIPv4, externalIPv4, internalIPv6, externalIPv6
}
