package operation

import "encoding/json"

func (c *Call) Decode(into any) error {
	if c == nil {
		return nilArgument("call")
	}
	if err := json.Unmarshal(c.Input, into); err != nil {
		return NewError(ErrorCodeInputInvalid, "the input cannot be read")
	}
	return nil
}

func JSONResult(data any) (Result, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return Result{}, wrapError(ErrorCodeFailed, "the result cannot be encoded", err)
	}
	return Result{Data: encoded}, nil
}
