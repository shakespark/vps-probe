// Package netaddr checks the host names and addresses written in config
// files, so that every config rejects the same mistakes with the same rule.
package netaddr

import (
	"net"
	"strconv"
	"strings"
)

// ValidHost reports whether s is an IP address or a host name, without port.
func ValidHost(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// ValidHostPort reports whether s is host:port with a valid host and a port
// from 1 to 65535: an address something can be sent to.
func ValidHostPort(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || !ValidHost(host) {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n > 0
}

// ValidListen reports whether s is [host]:port, an address to listen on: the
// host may be empty (every address) and the port 0 (any free one).
func ValidListen(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host != "" && !ValidHost(host) {
		return false
	}
	_, err = strconv.ParseUint(port, 10, 16)
	return err == nil
}
