package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	emailMaxHostBytes     = 253
	emailMaxUsernameBytes = 256
	emailMaxFolderBytes   = 255
	emailMaxPort          = 65535
)

var emailHostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

func validateEmailShapes(doc *yamlv3.Node) error {
	emailNode := channelNode(doc, "email")
	if emailNode == nil {
		return nil
	}
	if emailNode.Kind != yamlv3.MappingNode {
		return fmt.Errorf("channels.email must be a mapping at line %d", emailNode.Line)
	}
	for i := 0; i+1 < len(emailNode.Content); i += 2 {
		key := emailNode.Content[i].Value
		value := emailNode.Content[i+1]
		switch key {
		case "enabled":
			if value.Kind != yamlv3.ScalarNode || value.Tag != "!!bool" {
				return fmt.Errorf("channels.email.enabled must be a boolean at line %d", value.Line)
			}
		case "max_attachment_bytes":
			if value.Kind != yamlv3.ScalarNode || value.Tag != "!!int" {
				return fmt.Errorf("channels.email.max_attachment_bytes must be an integer at line %d", value.Line)
			}
		case "accounts":
			if err := validateEmailAccountShapes(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateEmailAccountShapes(accountsNode *yamlv3.Node) error {
	if accountsNode.Kind != yamlv3.SequenceNode {
		return fmt.Errorf("channels.email.accounts must be a sequence at line %d", accountsNode.Line)
	}
	for index, accountNode := range accountsNode.Content {
		field := fmt.Sprintf("channels.email.accounts[%d]", index)
		if accountNode.Kind != yamlv3.MappingNode {
			return fmt.Errorf("%s must be a mapping at line %d", field, accountNode.Line)
		}
		for i := 0; i+1 < len(accountNode.Content); i += 2 {
			key := accountNode.Content[i].Value
			value := accountNode.Content[i+1]
			switch key {
			case "name", "persona", "address", "authserv_id":
				if value.Kind != yamlv3.ScalarNode || value.Tag != "!!str" {
					return fmt.Errorf("%s.%s must be a string at line %d", field, key, value.Line)
				}
			case "allowed_senders":
				if err := quotedStringSequence(field+"."+key, value); err != nil {
					return err
				}
			case "imap", "smtp":
				if err := validateEmailEndpointShape(field+"."+key, value); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateEmailEndpointShape(field string, node *yamlv3.Node) error {
	if node.Kind != yamlv3.MappingNode {
		return fmt.Errorf("%s must be a mapping at line %d", field, node.Line)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := node.Content[i+1]
		tag, kind := "!!str", "a string"
		switch key {
		case "host", "username", "password_ref", "folder":
		case "poll_interval":
			kind = "a duration string"
		case "port":
			tag, kind = "!!int", "an integer"
		default:
			continue
		}
		if value.Kind != yamlv3.ScalarNode || value.Tag != tag {
			return fmt.Errorf("%s.%s must be %s at line %d", field, key, kind, value.Line)
		}
	}
	return nil
}

func (*EmailAccount) Default() any {
	return EmailAccount{
		IMAP: EmailIMAP{
			Port:         EmailDefaultIMAPPort,
			Folder:       EmailDefaultFolder,
			PollInterval: configkit.Duration(EmailDefaultPollInterval),
		},
		SMTP: EmailSMTP{Port: EmailDefaultSMTPPort},
	}
}

func validateEmail(email *Email, owner *Owner) error {
	if email == nil {
		return configkit.Invalid("channels.email", "must not be nil")
	}
	var problems []error
	names := make(map[string]bool, len(email.Accounts))
	for i := range email.Accounts {
		account := &email.Accounts[i]
		field := fmt.Sprintf("channels.email.accounts[%d]", i)
		switch {
		case !discordAccountNamePattern.MatchString(account.Name):
			problems = append(problems, configkit.Invalid(field+".name", fmt.Sprintf("%q must match %s", account.Name, discordAccountNamePattern.String())))
		case names[account.Name]:
			problems = append(problems, configkit.Invalid(field+".name", fmt.Sprintf("%q is configured more than once", account.Name)))
		default:
			names[account.Name] = true
		}
		problems = append(problems, emailAccountProblems(field, account, email.Enabled)...)
	}
	if email.Enabled && len(email.Accounts) == 0 {
		problems = append(problems, configkit.Invalid("channels.email.enabled", "requires at least one channels.email.accounts entry"))
	}
	if email.Enabled && !hasEmailOwner(owner) {
		for i := range email.Accounts {
			if len(email.Accounts[i].AllowedSenders) == 0 {
				problems = append(problems, configkit.Invalid("channels.email.enabled", "requires an email:<address> entry in owner.identities or an allowed_senders entry on every account"))
				break
			}
		}
	}
	if email.MaxAttachmentBytes < 1 || email.MaxAttachmentBytes > EmailMaxAttachmentBytes {
		problems = append(problems, configkit.Invalid("channels.email.max_attachment_bytes", fmt.Sprintf("must be between 1 and %d", EmailMaxAttachmentBytes)))
	}
	return errors.Join(problems...)
}

func emailAccountProblems(field string, account *EmailAccount, enabled bool) []error {
	var problems []error
	if account.Persona == "" {
		problems = append(problems, configkit.Invalid(field+".persona", "must not be empty"))
	}
	if !principal.IsPlainEmailAddress(account.Address) {
		problems = append(problems, configkit.Invalid(field+".address", "must be a lowercase mailbox address"))
	}
	if !emailHostName(account.AuthservID) {
		problems = append(problems, configkit.Invalid(field+".authserv_id", "must be the receiving server's host name"))
	}
	problems = append(problems, emailIMAPProblems(field+".imap", &account.IMAP)...)
	if enabled || account.SMTP.Host != "" || account.SMTP.Username != "" || account.SMTP.PasswordRef != "" {
		problems = append(problems, emailSMTPProblems(field+".smtp", &account.SMTP)...)
	}
	seen := make(map[string]bool, len(account.AllowedSenders))
	for _, sender := range account.AllowedSenders {
		switch {
		case !principal.IsEmailAddress(sender):
			problems = append(problems, configkit.Invalid(field+".allowed_senders", "entries must be lowercase mailbox addresses"))
		case seen[sender]:
			problems = append(problems, configkit.Invalid(field+".allowed_senders", "lists an address more than once"))
		default:
			seen[sender] = true
		}
	}
	return problems
}

func emailIMAPProblems(field string, imap *EmailIMAP) []error {
	problems := emailEndpointProblems(field, imap.Host, imap.Port, imap.Username, imap.PasswordRef)
	if !emailPlainText(imap.Folder, emailMaxFolderBytes) {
		problems = append(problems, configkit.Invalid(field+".folder", fmt.Sprintf("must be a single line of at most %d bytes", emailMaxFolderBytes)))
	}
	interval := time.Duration(imap.PollInterval)
	if interval < EmailMinPollInterval || interval > EmailMaxPollInterval {
		problems = append(problems, configkit.Invalid(field+".poll_interval", fmt.Sprintf("must be between %s and %s", EmailMinPollInterval, EmailMaxPollInterval)))
	}
	return problems
}

func emailSMTPProblems(field string, smtp *EmailSMTP) []error {
	return emailEndpointProblems(field, smtp.Host, smtp.Port, smtp.Username, smtp.PasswordRef)
}

func emailEndpointProblems(field, host string, port int, username, passwordRef string) []error {
	var problems []error
	if !emailHostName(host) {
		problems = append(problems, configkit.Invalid(field+".host", "must be a host name"))
	}
	if port < 1 || port > emailMaxPort {
		problems = append(problems, configkit.Invalid(field+".port", fmt.Sprintf("must be between 1 and %d", emailMaxPort)))
	}
	if !emailPlainText(username, emailMaxUsernameBytes) {
		problems = append(problems, configkit.Invalid(field+".username", fmt.Sprintf("must be a single line of at most %d bytes", emailMaxUsernameBytes)))
	}
	switch {
	case passwordRef == "":
		problems = append(problems, configkit.Invalid(field+".password_ref", "must not be empty"))
	case !secretReferenceShaped(passwordRef):
		problems = append(problems, configkit.Invalid(field+".password_ref", "must use env:// or file://"))
	}
	return problems
}

func secretReferenceShaped(raw string) bool {
	for _, scheme := range []string{"env://", "file://"} {
		if rest, found := strings.CutPrefix(raw, scheme); found {
			return rest != ""
		}
	}
	return false
}

func emailHostName(host string) bool {
	return len(host) <= emailMaxHostBytes && emailHostPattern.MatchString(host)
}

func emailPlainText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	return strings.IndexFunc(value, unicode.IsControl) < 0
}

func hasEmailOwner(owner *Owner) bool {
	if owner == nil {
		return false
	}
	for _, identity := range owner.Identities {
		if channel, _, found := strings.Cut(identity, ":"); found && channel == principal.ChannelEmail {
			return true
		}
	}
	return false
}
