package configkit

import (
	"fmt"
	"regexp"
)

var AccountNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func ValidateAccountName(field, name string) error {
	if !AccountNamePattern.MatchString(name) {
		return Invalid(field, fmt.Sprintf("%q must match %s", name, AccountNamePattern.String()))
	}
	return nil
}
