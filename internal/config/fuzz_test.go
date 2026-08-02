package config

import "testing"

// FuzzConfigDecode drives the config shape validation and decoder with
// arbitrary bytes. Both stages must return an error or a result and never
// panic, so a hostile or corrupt config file cannot crash the process at load.
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
