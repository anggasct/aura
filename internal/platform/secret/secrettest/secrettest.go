package secrettest

import (
	"github.com/anggasct/aura/internal/platform/secret"
)

func Ref(text string) secret.Ref {
	ref, err := secret.ParseRef(text)
	if err != nil {
		panic(err)
	}
	return ref
}
