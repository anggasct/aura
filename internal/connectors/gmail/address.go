package gmail

import (
	"regexp"
	"strings"
)

const (
	atext        = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!#$%&'*+-/=?^_`{|}~"
	maxLabelSize = 63
	maxDomain    = 253
)

var (
	idExpression        = regexp.MustCompile(idPattern)
	messageIDExpression = regexp.MustCompile("^<[A-Za-z0-9!#$%&'*+/=?^_`{|}~.-]{1,200}@[A-Za-z0-9.-]{1,200}>$")
)

func validID(id string) bool {
	return idExpression.MatchString(id)
}

func validMessageID(id string) bool {
	return len(id) <= maxMessageIDBytes && messageIDExpression.MatchString(id) && !strings.Contains(id, "..")
}

func validAddress(address string) bool {
	if len(address) > maxAddressBytes {
		return false
	}
	local, domain, found := strings.Cut(address, "@")
	if !found || strings.Contains(domain, "@") {
		return false
	}
	return validLocal(local) && validDomain(domain)
}

func validLocal(local string) bool {
	if local == "" || len(local) > maxLocalBytes || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") || strings.Contains(local, "=?") {
		return false
	}
	for i := range len(local) {
		if local[i] != '.' && !strings.ContainsRune(atext, rune(local[i])) {
			return false
		}
	}
	return true
}

func validDomain(domain string) bool {
	if domain == "" || len(domain) > maxDomain {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if !validLabel(label) {
			return false
		}
	}
	return !allDigits(labels[len(labels)-1])
}

func validLabel(label string) bool {
	if label == "" || len(label) > maxLabelSize || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := range len(label) {
		c := label[i]
		if c != '-' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

func allDigits(text string) bool {
	for i := range len(text) {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return text != ""
}
