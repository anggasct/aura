package turn

import "testing"

func TestOriginUnattended(t *testing.T) {
	cases := map[Origin]bool{
		OriginSchedule: true,
		OriginHTTP:     true,
		OriginTerminal: false,
		OriginChild:    false,
		OriginWorkflow: false,
		OriginWebhook:  false,
		"telegram:me":  false,
		"":             false,
	}
	for origin, want := range cases {
		t.Run(string(origin), func(t *testing.T) {
			if got := origin.Unattended(); got != want {
				t.Fatalf("Origin(%q).Unattended() = %t, want %t", origin, got, want)
			}
		})
	}
}

func TestOriginOnChannel(t *testing.T) {
	cases := []struct {
		name    string
		origin  Origin
		channel string
		want    bool
	}{
		{"the bare channel", "telegram", "telegram", true},
		{"an account of the channel", "telegram:main", "telegram", true},
		{"another channel", "discord:main", "telegram", false},
		{"a lookalike prefix", "telegramx:main", "telegram", false},
		{"an empty origin", "", "telegram", false},
		{"an empty channel", "telegram:main", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.origin.OnChannel(tc.channel); got != tc.want {
				t.Fatalf("Origin(%q).OnChannel(%q) = %t, want %t", tc.origin, tc.channel, got, tc.want)
			}
		})
	}
}
