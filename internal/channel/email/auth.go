package email

import (
	"errors"
	"strings"

	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-msgauth/authres"
)

const authResultsKey = "Authentication-Results"

var errUnbalancedComment = errors.New("email: authentication results comment is not closed")

func authenticatedSender(header *textproto.Header, authservID, fromDomain string) bool {
	if authservID == "" || fromDomain == "" {
		return false
	}
	fields := header.Fields()
	for fields.Next() {
		if !strings.EqualFold(fields.Key(), authResultsKey) {
			continue
		}
		value, err := stripComments(fields.Value())
		if err != nil {
			return false
		}
		if !strings.EqualFold(serverIDOf(value), authservID) {
			continue
		}
		return dmarcPasses(value, fromDomain)
	}
	return false
}

func serverIDOf(value string) string {
	head, _, _ := strings.Cut(value, ";")
	words := strings.Fields(head)
	if len(words) == 0 {
		return ""
	}
	return words[0]
}

func dmarcPasses(value, fromDomain string) bool {
	_, results, err := authres.Parse(value)
	if err != nil {
		return false
	}
	passed := false
	for _, result := range results {
		dmarc, ok := result.(*authres.DMARCResult)
		if !ok {
			continue
		}
		named := dmarc.From == "" || sameDomain(dmarc.From, fromDomain)
		switch {
		case !named:
		case dmarc.Value == authres.ResultPass:
			passed = true
		default:
			return false
		}
	}
	return passed
}

func sameDomain(claimed, actual string) bool {
	if _, domain, found := strings.Cut(claimed, "@"); found {
		claimed = domain
	}
	return strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(claimed), "."), strings.TrimSuffix(actual, "."))
}

func stripComments(value string) (string, error) {
	var out strings.Builder
	depth := 0
	quoted := false
	escaped := false
	for _, r := range value {
		switch {
		case escaped:
			escaped = false
			if depth == 0 {
				out.WriteRune(r)
			}
		case r == '\\':
			escaped = true
			if depth == 0 {
				out.WriteRune(r)
			}
		case quoted:
			quoted = r != '"'
			out.WriteRune(r)
		case r == '"' && depth == 0:
			quoted = true
			out.WriteRune(r)
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
			if depth == 0 {
				out.WriteRune(' ')
			}
		case depth == 0:
			out.WriteRune(r)
		}
	}
	if depth != 0 || quoted {
		return "", errUnbalancedComment
	}
	return out.String(), nil
}
