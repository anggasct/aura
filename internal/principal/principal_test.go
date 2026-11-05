package principal

import (
	"strings"
	"testing"
)

func TestResolverResolve(t *testing.T) {
	owners, err := NewResolver([]string{"discord:777"})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	cases := map[string]struct {
		externalID   string
		allowlisted  bool
		servesPublic bool
		id           string
		class        Class
		ok           bool
	}{
		"owner identity":                       {externalID: "777", id: OwnerID, class: ClassOwner, ok: true},
		"owner identity also allowlisted":      {externalID: "777", allowlisted: true, id: OwnerID, class: ClassOwner, ok: true},
		"allowlisted user":                     {externalID: "111", allowlisted: true, id: "discord:111", class: ClassTrusted, ok: true},
		"unknown user":                         {externalID: "666"},
		"unknown user on a public account":     {externalID: "666", servesPublic: true, id: "discord:666", class: ClassPublic, ok: true},
		"allowlisted user on a public account": {externalID: "111", allowlisted: true, servesPublic: true, id: "discord:111", class: ClassTrusted, ok: true},
		"owner on a public account":            {externalID: "777", servesPublic: true, id: OwnerID, class: ClassOwner, ok: true},
		"empty id":                             {externalID: "", allowlisted: true, servesPublic: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			id, class, ok := owners.Resolve(ChannelDiscord, tc.externalID, tc.allowlisted, tc.servesPublic)
			if id != tc.id || class != tc.class || ok != tc.ok {
				t.Errorf("Resolve() = %q, %q, %v; want %q, %q, %v", id, class, ok, tc.id, tc.class, tc.ok)
			}
		})
	}
}

func TestNilResolverAdmitsOnlyAllowlisted(t *testing.T) {
	var owners *Resolver
	if id, class, ok := owners.Resolve(ChannelDiscord, "111", true, false); id != "discord:111" || class != ClassTrusted || !ok {
		t.Errorf("Resolve() = %q, %q, %v", id, class, ok)
	}
	if _, _, ok := owners.Resolve(ChannelDiscord, "111", false, false); ok {
		t.Error("nil resolver admitted a non-allowlisted user")
	}
	if owners.Count(ChannelDiscord) != 0 || owners.IsOwnerIdentity(ChannelDiscord, "111") {
		t.Error("nil resolver reports owner identities")
	}
}

func TestOwnerExternalIDsKeepConfigOrder(t *testing.T) {
	owners, err := NewResolver([]string{"discord:900", "discord:42"})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if got := owners.OwnerExternalIDs(ChannelDiscord); len(got) != 2 || got[0] != "900" || got[1] != "42" {
		t.Errorf("OwnerExternalIDs() = %v, want [900 42]", got)
	}
	var none *Resolver
	if got := none.OwnerExternalIDs(ChannelDiscord); got != nil {
		t.Errorf("nil resolver OwnerExternalIDs() = %v", got)
	}
}

func TestNewResolverRejectsInvalidIdentities(t *testing.T) {
	cases := map[string]struct {
		identities []string
		index      string
	}{
		"unknown channel":      {identities: []string{"slack:123"}, index: "owner.identities[0]"},
		"empty telegram id":    {identities: []string{"telegram:"}, index: "owner.identities[0]"},
		"negative telegram id": {identities: []string{"telegram:-42"}, index: "owner.identities[0]"},
		"zero telegram id":     {identities: []string{"discord:42", "telegram:0"}, index: "owner.identities[1]"},
		"padded telegram id":   {identities: []string{"telegram:0042"}, index: "owner.identities[0]"},
		"signed telegram id":   {identities: []string{"telegram:+42"}, index: "owner.identities[0]"},
		"telegram overflow":    {identities: []string{"telegram:9223372036854775808"}, index: "owner.identities[0]"},
		"terminal is implicit": {identities: []string{"discord:900", "terminal:me"}, index: "owner.identities[1]"},
		"http key principal":   {identities: []string{"http:abcdefghijkl"}, index: "owner.identities[0]"},
		"missing separator":    {identities: []string{"123456"}, index: "owner.identities[0]"},
		"empty discord id":     {identities: []string{"discord:"}, index: "owner.identities[0]"},
		"non-numeric id":       {identities: []string{"discord:12ab"}, index: "owner.identities[0]"},
		"overlong id":          {identities: []string{"discord:123456789012345678901"}, index: "owner.identities[0]"},
		"duplicate":            {identities: []string{"discord:42", "discord:42"}, index: "owner.identities[1]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewResolver(tc.identities)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeIdentityInvalid {
				t.Fatalf("code = %q, %v; want %q (err %v)", code, ok, ErrorCodeIdentityInvalid, err)
			}
			if !strings.Contains(err.Error(), tc.index) {
				t.Errorf("error %q does not name %s", err, tc.index)
			}
			for _, identity := range tc.identities {
				if _, value, found := strings.Cut(identity, ":"); found && value != "" && strings.Contains(err.Error(), value) {
					t.Errorf("error %q leaks identity value", err)
				}
			}
		})
	}
}

func TestNewResolverAggregatesProblems(t *testing.T) {
	_, err := NewResolver([]string{"telegram:x", "discord:x"})
	for _, index := range []string{"owner.identities[0]", "owner.identities[1]"} {
		if err == nil || !strings.Contains(err.Error(), index) {
			t.Errorf("error %v does not name %s", err, index)
		}
	}
}

func TestResolverKeepsChannelsApart(t *testing.T) {
	owners, err := NewResolver([]string{"telegram:777", "discord:900"})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if id, class, ok := owners.Resolve(ChannelTelegram, "777", false, false); id != OwnerID || class != ClassOwner || !ok {
		t.Fatalf("telegram owner = %q %q %v", id, class, ok)
	}
	if _, _, ok := owners.Resolve(ChannelTelegram, "900", false, false); ok {
		t.Fatal("a discord owner id admitted on telegram")
	}
	if owners.Count(ChannelTelegram) != 1 || owners.Count(ChannelDiscord) != 1 {
		t.Fatalf("counts telegram=%d discord=%d", owners.Count(ChannelTelegram), owners.Count(ChannelDiscord))
	}
}

func TestParseClassFailsClosed(t *testing.T) {
	for raw, want := range map[string]Class{
		"owner":   ClassOwner,
		"trusted": ClassTrusted,
		"public":  ClassPublic,
		"":        ClassTrusted,
		"admin":   ClassTrusted,
		"OWNER":   ClassTrusted,
	} {
		if got := ParseClass(raw); got != want {
			t.Errorf("ParseClass(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestClassForPrincipal(t *testing.T) {
	if ClassForPrincipal(OwnerID) != ClassOwner {
		t.Error("canonical owner is not owner class")
	}
	for _, id := range []string{"cron", "discord:1", "webhook:ci", ""} {
		if ClassForPrincipal(id) != ClassTrusted {
			t.Errorf("ClassForPrincipal(%q) is not trusted", id)
		}
	}
}

func TestTelegramIDShapes(t *testing.T) {
	cases := []struct {
		id                 string
		user, topic, group bool
	}{
		{"7", true, true, false},
		{"-1001234567890", false, false, true},
		{"0", false, false, false},
		{"-0", false, false, false},
		{"-007", false, false, false},
		{"+7", false, false, false},
		{"7.0", false, false, false},
		{"", false, false, false},
		{"abc", false, false, false},
		{"9223372036854775808", false, false, false},
		{"-9223372036854775809", false, false, false},
	}
	for _, tc := range cases {
		if got := IsTelegramUserID(tc.id); got != tc.user {
			t.Errorf("IsTelegramUserID(%q) = %v, want %v", tc.id, got, tc.user)
		}
		if got := IsTelegramTopicID(tc.id); got != tc.topic {
			t.Errorf("IsTelegramTopicID(%q) = %v, want %v", tc.id, got, tc.topic)
		}
		if got := IsTelegramGroupChatID(tc.id); got != tc.group {
			t.Errorf("IsTelegramGroupChatID(%q) = %v, want %v", tc.id, got, tc.group)
		}
	}
}

func TestParseIdentityHTTPKeyPrincipal(t *testing.T) {
	cases := map[string]struct {
		raw     string
		channel string
		id      string
		valid   bool
	}{
		"key id":            {raw: "http:abcdefghijkl", channel: ChannelHTTP, id: "abcdefghijkl", valid: true},
		"key id with 2-7":   {raw: "http:a2b3c4d5e6f7", channel: ChannelHTTP, id: "a2b3c4d5e6f7", valid: true},
		"too short":         {raw: "http:abcdefghijk"},
		"too long":          {raw: "http:abcdefghijklm"},
		"uppercase":         {raw: "http:ABCDEFGHIJKL"},
		"digit outside 2-7": {raw: "http:abcdefghij01"},
		"separator inside":  {raw: "http:abcdefg/ijkl"},
		"empty":             {raw: "http:"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			channel, id, err := ParseIdentity(tc.raw)
			if !tc.valid {
				if code, ok := CodeOf(err); !ok || code != ErrorCodeIdentityInvalid {
					t.Fatalf("code = %q, %v; want %q", code, ok, ErrorCodeIdentityInvalid)
				}
				return
			}
			if err != nil || channel != tc.channel || id != tc.id {
				t.Fatalf("ParseIdentity() = %q, %q, %v", channel, id, err)
			}
		})
	}
}
