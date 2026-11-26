package email

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	MaxAttachmentBytes  int64 = 20971520
	DefaultIMAPPort           = 993
	DefaultSMTPPort           = 587
	DefaultFolder             = "INBOX"
	DefaultPollInterval       = time.Minute
	MinPollInterval           = 10 * time.Second
	MaxPollInterval           = time.Hour
)

const (
	maxHostBytes     = 253
	maxUsernameBytes = 256
	maxFolderBytes   = 255
	maxPort          = 65535
)

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

type Config struct {
	Enabled            bool      `json:"enabled"`
	Accounts           []Account `json:"accounts"`
	MaxAttachmentBytes int64     `json:"max_attachment_bytes"`
}

type Account struct {
	Name           string     `json:"name"`
	Persona        string     `json:"persona"`
	Address        string     `json:"address"`
	IMAP           IMAPConfig `json:"imap"`
	SMTP           SMTPConfig `json:"smtp"`
	AuthservID     string     `json:"authserv_id"`
	AllowedSenders []string   `json:"allowed_senders"`
}

type SMTPConfig struct {
	Host        string     `json:"host"`
	Port        int        `json:"port"`
	Username    string     `json:"username"`
	PasswordRef secret.Ref `json:"password_ref"`
}

type IMAPConfig struct {
	Host         string             `json:"host"`
	Port         int                `json:"port"`
	Username     string             `json:"username"`
	PasswordRef  secret.Ref         `json:"password_ref"`
	Folder       string             `json:"folder"`
	PollInterval configkit.Duration `json:"poll_interval"`
}

func Default() Config {
	return Config{
		MaxAttachmentBytes: MaxAttachmentBytes,
	}
}

func (*Account) Default() any {
	return Account{
		IMAP: IMAPConfig{
			Port:         DefaultIMAPPort,
			Folder:       DefaultFolder,
			PollInterval: configkit.Duration(DefaultPollInterval),
		},
		SMTP: SMTPConfig{Port: DefaultSMTPPort},
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return configkit.Invalid("", "must not be nil")
	}
	var problems []error
	names := make(map[string]bool, len(c.Accounts))
	for i := range c.Accounts {
		account := &c.Accounts[i]
		field := fmt.Sprintf("accounts[%d]", i)
		if err := configkit.ValidateAccountName(field+".name", account.Name); err != nil {
			problems = append(problems, err)
		} else if names[account.Name] {
			problems = append(problems, configkit.Invalid(field+".name", fmt.Sprintf("%q is configured more than once", account.Name)))
		} else {
			names[account.Name] = true
		}
		problems = append(problems, accountProblems(field, account, c.Enabled)...)
	}
	if c.Enabled && len(c.Accounts) == 0 {
		problems = append(problems, configkit.Invalid("enabled", "requires at least one channels.email.accounts entry"))
	}
	if c.MaxAttachmentBytes < 1 || c.MaxAttachmentBytes > MaxAttachmentBytes {
		problems = append(problems, configkit.Invalid("max_attachment_bytes", fmt.Sprintf("must be between 1 and %d", MaxAttachmentBytes)))
	}
	return errors.Join(problems...)
}

func accountProblems(field string, account *Account, enabled bool) []error {
	var problems []error
	if account.Persona == "" {
		problems = append(problems, configkit.Invalid(field+".persona", "must not be empty"))
	}
	if !principal.IsPlainEmailAddress(account.Address) {
		problems = append(problems, configkit.Invalid(field+".address", "must be a lowercase mailbox address"))
	}
	if !validHostName(account.AuthservID) {
		problems = append(problems, configkit.Invalid(field+".authserv_id", "must be the receiving server's host name"))
	}
	problems = append(problems, imapProblems(field+".imap", &account.IMAP)...)
	if enabled || account.SMTP.Host != "" || account.SMTP.Username != "" || !account.SMTP.PasswordRef.IsZero() {
		problems = append(problems, smtpProblems(field+".smtp", &account.SMTP)...)
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

func imapProblems(field string, imap *IMAPConfig) []error {
	problems := endpointProblems(field, imap.Host, imap.Port, imap.Username, imap.PasswordRef)
	if !validPlainText(imap.Folder, maxFolderBytes) {
		problems = append(problems, configkit.Invalid(field+".folder", fmt.Sprintf("must be a single line of at most %d bytes", maxFolderBytes)))
	}
	interval := time.Duration(imap.PollInterval)
	if interval < MinPollInterval || interval > MaxPollInterval {
		problems = append(problems, configkit.Invalid(field+".poll_interval", fmt.Sprintf("must be between %s and %s", MinPollInterval, MaxPollInterval)))
	}
	return problems
}

func smtpProblems(field string, smtp *SMTPConfig) []error {
	return endpointProblems(field, smtp.Host, smtp.Port, smtp.Username, smtp.PasswordRef)
}

func endpointProblems(field, host string, port int, username string, passwordRef secret.Ref) []error {
	var problems []error
	if !validHostName(host) {
		problems = append(problems, configkit.Invalid(field+".host", "must be a host name"))
	}
	if port < 1 || port > maxPort {
		problems = append(problems, configkit.Invalid(field+".port", fmt.Sprintf("must be between 1 and %d", maxPort)))
	}
	if !validPlainText(username, maxUsernameBytes) {
		problems = append(problems, configkit.Invalid(field+".username", fmt.Sprintf("must be a single line of at most %d bytes", maxUsernameBytes)))
	}
	if passwordRef.IsZero() {
		problems = append(problems, configkit.Invalid(field+".password_ref", "must not be empty"))
	}
	return problems
}

func validHostName(host string) bool {
	return len(host) <= maxHostBytes && hostPattern.MatchString(host)
}

func validPlainText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	return strings.IndexFunc(value, unicode.IsControl) < 0
}
