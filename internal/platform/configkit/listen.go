package configkit

import (
	"net"
	"strconv"
)

func ValidateListenAddress(field, address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return Invalid(field, "must be host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return Invalid(field, "port must be between 1 and 65535")
	}
	if host == "" {
		return Invalid(field, "must include a host")
	}
	return nil
}
