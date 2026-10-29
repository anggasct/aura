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
		externalID  string
		allowlisted bool
		id          string
		class       Class
		ok          bool
	}{
		"owner identity":                  {externalID: "777", id: OwnerID, class: ClassOwner, ok: true},
		"owner identity also allowlisted": {externalID: "777", allowlisted: true, id: OwnerID, class: ClassOwner, ok: true},
		"allowlisted user":                {externalID: "111", allowlisted: true, id: "discord:111", class: ClassTrusted, ok: true},
		"unknown user":                    {externalID: "666"},
		"empty id":                        {externalID: "", allowlisted: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			id, class, ok := owners.Resolve(ChannelDiscord, tc.externalID, tc.allowlisted)
			if id != tc.id || class != tc.class || ok != tc.ok {
				t.Errorf("Resolve() = %q, %q, %v; want %q, %q, %v", id, class, ok, tc.id, tc.class, tc.ok)
			}
		})
	}
}

func TestNilResolverAdmitsOnlyAllowlisted(t *testing.T) {
	var owners *Resolver
	if id, class, ok := owners.Resolve(ChannelDiscord, "111", true); id != "discord:111" || class != ClassTrusted || !ok {
		t.Errorf("Resolve() = %q, %q, %v", id, class, ok)
	}
	if _, _, ok := owners.Resolve(ChannelDiscord, "111", false); ok {
		t.Error("nil resolver admitted a non-allowlisted user")
	}
	if owners.Count(ChannelDiscord) != 0 || owners.IsOwnerIdentity(ChannelDiscord, "111") {
		t.Error("nil resolver reports owner identities")
	}
}

func TestNewResolverRejectsInvalidIdentities(t *testing.T) {
	cases := map[string]struct {
		identities []string
		index      string
	}{
		"unknown channel":      {identities: []string{"telegram:123"}, index: "owner.identities[0]"},
		"terminal is implicit": {identities: []string{"discord:900", "terminal:me"}, index: "owner.identities[1]"},
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
	_, err := NewResolver([]string{"telegram:1", "discord:x"})
	for _, index := range []string{"owner.identities[0]", "owner.identities[1]"} {
		if err == nil || !strings.Contains(err.Error(), index) {
			t.Errorf("error %v does not name %s", err, index)
		}
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
