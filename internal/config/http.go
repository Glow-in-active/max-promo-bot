package config

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
)

func HTTPAddress() (string, error) {
	address := env("HTTP_ADDR", ":8080")
	return address, validateHTTPAddress(address)
}

func validateHTTPAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || strings.ContainsAny(host, "/?#@[] \t\r\n") {
		return validationError("HTTP_ADDR must be a host:port address")
	}
	if strings.Contains(host, ":") {
		if _, err := netip.ParseAddr(host); err != nil {
			return validationError("HTTP_ADDR must contain a valid IP address or hostname")
		}
	}
	for _, digit := range port {
		if digit < '0' || digit > '9' {
			return validationError("HTTP_ADDR port must be an integer between 1 and 65535")
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return validationError("HTTP_ADDR port must be an integer between 1 and 65535")
	}
	return nil
}
