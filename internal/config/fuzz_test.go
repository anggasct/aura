package config

import "testing"

func FuzzConfigDecode(f *testing.F) {
	f.Add([]byte("version: 1\n"))
	f.Add([]byte("{not yaml"))
	f.Add([]byte(""))
	f.Add([]byte("version: 1\nruntime:\n  max_active_turns: -1\n"))
	f.Add([]byte("::: bad\n\tindent"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = validate(data)
		_, _ = decode(data)
	})
}
