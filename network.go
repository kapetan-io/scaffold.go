package scaffold

import (
	"fmt"
	"net"
)

// resolveBindAddress validates and resolves the bind address from Options.
// If raw is non-empty, it is validated as an IP address. If raw is empty,
// the default is "127.0.0.1" for test bindings and "0.0.0.0" for production.
// Returns the resolved address, the source label, and any validation error.
func resolveBindAddress(raw string, isTestBindings bool) (addr string, source string, err error) {
	if raw != "" {
		if net.ParseIP(raw) == nil {
			return "", "", fmt.Errorf("scaffold: invalid BindAddress %q: not a valid IP address", raw)
		}
		return raw, "configured", nil
	}
	if isTestBindings {
		return "127.0.0.1", "default", nil
	}
	return "0.0.0.0", "default", nil
}

// resolveAdvertisedAddress validates and resolves the advertised address.
// If raw is non-empty, it is validated as an IP address and returned.
// If raw is empty and bindAddr is not a wildcard, bindAddr is used as the
// advertised address. If bindAddr is a wildcard, the address is auto-detected
// from the network interfaces.
// Returns the resolved address, the source label, and any error.
func resolveAdvertisedAddress(bindAddr, raw string) (addr string, source string, err error) {
	if raw != "" {
		if net.ParseIP(raw) == nil {
			return "", "", fmt.Errorf("scaffold: invalid AdvertisedAddress %q: not a valid IP address", raw)
		}
		return raw, "configured", nil
	}
	if !isWildcard(bindAddr) {
		return bindAddr, "derived", nil
	}
	// bindAddr is a wildcard — derive the address family and auto-detect.
	var family int
	switch bindAddr {
	case "::", "0:0:0:0:0:0:0:0":
		family = 6
	default:
		// "0.0.0.0" and any other wildcard defaults to IPv4.
		family = 4
	}
	detected, err := autoDetectAddress(family)
	if err != nil {
		return "", "", err
	}
	return detected, "auto-detected", nil
}

// autoDetectAddress enumerates network interfaces and returns the first
// private, non-loopback, non-link-local address of the given address family
// (4 for IPv4, 6 for IPv6). Returns an error if no suitable address is found.
func autoDetectAddress(family int) (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("scaffold: failed to enumerate network interfaces: %w", err)
	}
	familyName := "IPv4"
	if family == 6 {
		familyName = "IPv6"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ip, _, err := net.ParseCIDR(a.String())
			if err != nil {
				continue
			}
			// Filter by address family.
			if family == 4 && ip.To4() == nil {
				continue
			}
			if family == 6 && ip.To4() != nil {
				continue
			}
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || !ip.IsPrivate() {
				continue
			}
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("scaffold: no private %s address found for advertised address auto-detection", familyName)
}

// isWildcard reports whether ip is a wildcard (all-zeros) address:
// "0.0.0.0" for IPv4, "::" or "0:0:0:0:0:0:0:0" for IPv6.
func isWildcard(ip string) bool {
	switch ip {
	case "0.0.0.0", "::", "0:0:0:0:0:0:0:0":
		return true
	}
	return false
}
