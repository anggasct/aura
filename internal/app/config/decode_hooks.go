package config

import (
	"encoding"
	"errors"
	"fmt"
	"math"
	"reflect"

	"github.com/go-viper/mapstructure/v2"
)

func textValue() mapstructure.DecodeHookFunc {
	return func(_, to reflect.Type, data any) (any, error) {
		if to.Kind() == reflect.Pointer {
			return data, nil
		}
		target := reflect.New(to)
		unmarshaler, isText := target.Interface().(encoding.TextUnmarshaler)
		if !isText {
			return data, nil
		}
		text, isString := data.(string)
		if !isString {
			return nil, fmt.Errorf("must be %s", writtenAsString(to))
		}
		if err := unmarshaler.UnmarshalText([]byte(text)); err != nil {
			return nil, err
		}
		return target.Elem().Interface(), nil
	}
}

func writtenAsString(t reflect.Type) string {
	if kind, known := textKinds[t]; known {
		return kind + " written as a string"
	}
	return "a string"
}

func integerValue() mapstructure.DecodeHookFunc {
	return func(_, to reflect.Type, data any) (any, error) {
		if to.Kind() < reflect.Int || to.Kind() > reflect.Uint64 {
			return data, nil
		}
		input := reflect.ValueOf(data)
		if !input.CanInt() && !input.CanUint() {
			return nil, errors.New("must be an integer")
		}
		if !integerFits(to, input) {
			return nil, fmt.Errorf("must be between %s", integerRange(to))
		}
		return data, nil
	}
}

var (
	signedWide   = reflect.TypeFor[int64]()
	unsignedWide = reflect.TypeFor[uint64]()
)

func integerFits(to reflect.Type, input reflect.Value) bool {
	target := reflect.Zero(to)
	switch {
	case input.CanInt() && target.CanInt():
		return !target.OverflowInt(input.Int())
	case input.CanInt():
		return input.Int() >= 0 && !target.OverflowUint(input.Convert(unsignedWide).Uint())
	case target.CanUint():
		return !target.OverflowUint(input.Uint())
	default:
		return input.Uint() <= math.MaxInt64 && !target.OverflowInt(input.Convert(signedWide).Int())
	}
}

func integerRange(t reflect.Type) string {
	shift := 64 - uint(t.Bits())
	if t.Kind() >= reflect.Uint {
		return fmt.Sprintf("0 and %d", uint64(math.MaxUint64)>>shift)
	}
	return fmt.Sprintf("%d and %d", int64(math.MinInt64)>>shift, int64(math.MaxInt64)>>shift)
}
