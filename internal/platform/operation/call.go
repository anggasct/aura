package operation

import (
	"context"
	"encoding/json"
	"strings"
)

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

func (c *Call) HasFile(param string) bool {
	if c == nil || c.run == nil {
		return false
	}
	_, ok := c.run.paths[param]
	return ok
}

func (c *Call) File(ctx context.Context, param string) (File, error) {
	if c == nil || c.run == nil || c.Files == nil {
		return nil, nilArgument("call")
	}
	source, ok := c.run.paths[param]
	if !ok {
		return nil, NewError(ErrorCodeInputInvalid, "parameter "+param+" is not set")
	}
	file, err := c.Files.Open(ctx, source)
	if err != nil {
		return nil, err
	}
	c.run.keep(file)
	return file, nil
}

func (n *invocation) keep(file File) {
	n.filesMu.Lock()
	defer n.filesMu.Unlock()
	n.opened = append(n.opened, file)
}

func (n *invocation) closeFiles() {
	n.filesMu.Lock()
	defer n.filesMu.Unlock()
	for _, file := range n.opened {
		_ = file.Close()
	}
	n.opened = nil
}

func isURLReference(value string) bool {
	return strings.Contains(value, "://")
}

func (d *Descriptor) extractPaths(surface Surface, input json.RawMessage) (json.RawMessage, map[string]string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(input, &object); err != nil {
		return nil, nil, wrapError(ErrorCodeInputInvalid, "the input is not a JSON object", err)
	}
	var paths map[string]string
	for _, param := range d.paramsOn(surface) {
		raw, present := object[param.Name]
		if param.Kind != KindPath || !present {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, nil, wrapError(ErrorCodeInputInvalid, "the path cannot be read", err)
		}
		delete(object, param.Name)
		if param.URLParam != "" && isURLReference(value) {
			object[param.URLParam] = raw
			continue
		}
		if paths == nil {
			paths = map[string]string{}
		}
		paths[param.Name] = value
	}
	if paths == nil && !d.hasPath(surface) {
		return input, nil, nil
	}
	rewritten, err := json.Marshal(object)
	if err != nil {
		return nil, nil, wrapError(ErrorCodeInputInvalid, "the input cannot be encoded", err)
	}
	return rewritten, paths, nil
}

func (d *Descriptor) hasPath(surface Surface) bool {
	for _, param := range d.paramsOn(surface) {
		if param.Kind == KindPath {
			return true
		}
	}
	return false
}
