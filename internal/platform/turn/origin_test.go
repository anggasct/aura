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
