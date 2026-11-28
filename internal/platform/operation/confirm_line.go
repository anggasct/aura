package operation

import (
	"encoding/json"
	"strings"
)

const confirmLinePrefix = "confirm with: "

func (r *Registry) confirmable(descriptor *Descriptor, call *Call, preview Preview) (Preview, error) {
	if !descriptor.confirmsWithCode() || preview.Empty || call.Caller.Surface == SurfaceAgent {
		return preview, nil
	}
	code, err := r.confirmer.Code(preview.Digest)
	if err != nil {
		return Preview{}, err
	}
	preview.Code = code
	preview.Lines = append(preview.Lines[:len(preview.Lines):len(preview.Lines)], confirmLinePrefix+confirmInvocation(descriptor, call, code))
	return preview, nil
}

func confirmInvocation(descriptor *Descriptor, call *Call, code string) string {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(call.Input, &values); err != nil {
		values = nil
	}
	surface := call.Caller.Surface
	words := commandWords(descriptor, surface)
	positionals := descriptor.Positionals(surface)
	for i := range positionals {
		if value, ok := plainValue(values[positionals[i].Param]); ok {
			words = append(words, value)
		}
	}
	flags := descriptor.Flags(surface)
	for i := range flags {
		flag := &flags[i]
		value, ok := plainValue(values[flag.Param])
		if !ok || value == defaultText(flag) {
			continue
		}
		if flag.Kind == KindBoolean {
			if value == "true" {
				words = append(words, flagPrefix+flag.Name)
			}
			continue
		}
		words = append(words, flagPrefix+flag.Name, value)
	}
	words = append(words, flagPrefix+flagApply, code)
	return strings.Join(words, " ")
}

func plainValue(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, true
	}
	return strings.TrimSpace(string(raw)), true
}

func defaultText(flag *Flag) string {
	text, _ := plainValue(flag.Default)
	return text
}
