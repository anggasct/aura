package configkit

import (
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestValidateAccountName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "single letter", input: "a"},
		{name: "alphanumeric with hyphen and underscore", input: "team_bot-1"},
		{name: "maximum length 64", input: "a" + strings.Repeat("b", 63)},
		{name: "empty", input: "", wantErr: true},
		{name: "uppercase first char", input: "Owner", wantErr: true},
		{name: "uppercase later char", input: "myBot", wantErr: true},
		{name: "leading digit", input: "1bot", wantErr: true},
		{name: "leading hyphen", input: "-bot", wantErr: true},
		{name: "leading underscore", input: "_bot", wantErr: true},
		{name: "exceeds 64 chars", input: "a" + strings.Repeat("b", 64), wantErr: true},
		{name: "contains colon", input: "bot:main", wantErr: true},
		{name: "contains slash", input: "bot/main", wantErr: true},
		{name: "contains space", input: "my bot", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAccountName("channels.discord.accounts[0].name", tc.input)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ValidateAccountName(%q) = %v, want nil", tc.input, err)
				}
				return
			}
			if !fault.HasCode(err, ErrorCodeInvalid) {
				t.Fatalf("ValidateAccountName(%q) = %v, want invalid code", tc.input, err)
			}
			var fieldErr *FieldError
			if !errors.As(err, &fieldErr) || fieldErr.Path != "channels.discord.accounts[0].name" {
				t.Errorf("ValidateAccountName(%q) field error = %v, want path channels.discord.accounts[0].name", tc.input, err)
			}
		})
	}
}
