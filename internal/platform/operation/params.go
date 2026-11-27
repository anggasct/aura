package operation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	schemaDialect    = "https://json-schema.org/draft/2020-12/schema"
	maxParams        = 16
	maxStringsItems  = 32
	maxPathLength    = 4096
	durationPattern  = `^\d+(ns|us|ms|s|m|h)(\d+(ns|us|ms|s|m|h))*$`
	schemaIDTemplate = "aura://operations/%s/v1"
)

var durationRule = regexp.MustCompile(durationPattern)

func SchemaID(id string) string {
	return fmt.Sprintf(schemaIDTemplate, id)
}

func encodeJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, wrapError(ErrorCodeInputInvalid, "a value cannot be encoded", err)
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func decodeValue(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("more than one value")
	}
	return value, nil
}

type objectWriter struct {
	buffer bytes.Buffer
	fields int
}

func newObjectWriter() *objectWriter {
	writer := &objectWriter{}
	writer.buffer.WriteByte('{')
	return writer
}

func (w *objectWriter) put(key string, value []byte) error {
	name, err := encodeJSON(key)
	if err != nil {
		return err
	}
	if w.fields > 0 {
		w.buffer.WriteByte(',')
	}
	w.fields++
	w.buffer.Write(name)
	w.buffer.WriteByte(':')
	w.buffer.Write(value)
	return nil
}

func (w *objectWriter) putValue(key string, value any) error {
	encoded, err := encodeJSON(value)
	if err != nil {
		return err
	}
	return w.put(key, encoded)
}

func (w *objectWriter) bytes() json.RawMessage {
	out := bytes.Clone(w.buffer.Bytes())
	return append(out, '}')
}

func compactJSON(raw json.RawMessage) ([]byte, error) {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return nil, wrapError(ErrorCodeInvalid, "a schema or default is not valid JSON", err)
	}
	return buffer.Bytes(), nil
}

func (p *Param) propertyJSON(surface Surface) (json.RawMessage, error) {
	if p.Kind == KindJSON {
		compact, err := compactJSON(p.Schema)
		if err != nil {
			return nil, err
		}
		return compact, nil
	}
	writer := newObjectWriter()
	put := func(key string, value any) error { return writer.putValue(key, value) }
	var err error
	switch p.Kind {
	case KindString, KindPath:
		err = firstError(put("type", "string"), p.putLengths(put))
	case KindInteger:
		err = firstError(put("type", "integer"), putBound(put, "minimum", p.Minimum), putBound(put, "maximum", p.Maximum))
	case KindBoolean:
		err = put("type", "boolean")
	case KindDuration:
		err = firstError(put("type", "string"), put("pattern", durationPattern))
	case KindEnum:
		err = firstError(put("type", "string"), put("enum", p.enumOn(surface)))
	case KindStrings:
		items := newObjectWriter()
		err = items.putValue("type", "string")
		if err == nil {
			err = putLengthsTo(items, p)
		}
		if err == nil {
			err = firstError(put("type", "array"), writer.put("items", items.bytes()), put("maxItems", maxStringsItems))
		}
	default:
		return nil, NewError(ErrorCodeInvalid, "parameter "+p.Name+" has an unknown kind")
	}
	if err == nil && p.Summary != "" {
		err = put("description", p.Summary)
	}
	if err == nil && len(p.Default) > 0 {
		var compact []byte
		if compact, err = compactJSON(p.Default); err == nil {
			err = writer.put("default", compact)
		}
	}
	if err != nil {
		return nil, err
	}
	return writer.bytes(), nil
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Param) putLengths(put func(key string, value any) error) error {
	minimum, maximum := p.MinLength, p.MaxLength
	if p.Kind == KindPath {
		minimum, maximum = 1, maxPathLength
	}
	if minimum > 0 {
		if err := put("minLength", minimum); err != nil {
			return err
		}
	}
	if maximum > 0 {
		return put("maxLength", maximum)
	}
	return nil
}

func putLengthsTo(writer *objectWriter, p *Param) error {
	return p.putLengths(writer.putValue)
}

func putBound(put func(key string, value any) error, key string, bound *int64) error {
	if bound == nil {
		return nil
	}
	return put(key, *bound)
}

func (d *Descriptor) SchemaFor(surface Surface) (json.RawMessage, error) {
	if d == nil {
		return nil, nilArgument("descriptor")
	}
	properties := newObjectWriter()
	var required []string
	for _, param := range d.paramsOn(surface) {
		property, err := param.propertyJSON(surface)
		if err != nil {
			return nil, err
		}
		if err := properties.put(param.Name, property); err != nil {
			return nil, err
		}
		if param.Required {
			required = append(required, param.Name)
		}
	}
	schema := newObjectWriter()
	err := firstError(
		schema.putValue("$schema", schemaDialect),
		schema.putValue("$id", SchemaID(d.ID)),
		schema.putValue("type", "object"),
		schema.put("properties", properties.bytes()),
	)
	if err == nil && len(required) > 0 {
		err = schema.putValue("required", required)
	}
	if err == nil {
		err = schema.putValue("additionalProperties", false)
	}
	if err != nil {
		return nil, err
	}
	return schema.bytes(), nil
}

func (d *Descriptor) Validate(surface Surface, raw json.RawMessage) (json.RawMessage, error) {
	if d == nil {
		return nil, nilArgument("descriptor")
	}
	if !surface.valid() {
		return nil, NewError(ErrorCodeInvalidArgument, "operation: surface is unknown")
	}
	object, err := tools.DecodeObject(raw)
	if err != nil {
		return nil, wrapError(ErrorCodeInputInvalid, "the input is not a JSON object", err)
	}
	params := d.paramsOn(surface)
	names := make([]string, 0, len(params))
	for _, param := range params {
		names = append(names, param.Name)
	}
	if err := tools.RejectUnknown(object, names...); err != nil {
		return nil, wrapError(ErrorCodeInputInvalid, "the input names a parameter this surface does not have", err)
	}
	if err := tools.RejectAmbiguousKeys(raw); err != nil {
		return nil, wrapError(ErrorCodeInputInvalid, "the input repeats a parameter", err)
	}
	canonical := newObjectWriter()
	for _, param := range params {
		value, present := object[param.Name]
		if !present {
			if len(param.Default) > 0 {
				decoded, err := decodeValue(param.Default)
				if err != nil {
					return nil, wrapError(ErrorCodeInvalid, "the default of parameter "+param.Name+" is not valid JSON", err)
				}
				value, present = decoded, true
			} else if param.Required {
				return nil, NewError(ErrorCodeInputInvalid, "parameter "+param.Name+" is required")
			}
		}
		if !present {
			continue
		}
		encoded, err := param.canonicalValue(surface, value)
		if err != nil {
			return nil, err
		}
		if err := canonical.put(param.Name, encoded); err != nil {
			return nil, err
		}
	}
	return canonical.bytes(), nil
}

func (p *Param) refusal(detail string) error {
	return NewError(ErrorCodeInputInvalid, "parameter "+p.Name+" "+detail)
}

func (p *Param) canonicalValue(surface Surface, value any) ([]byte, error) {
	switch p.Kind {
	case KindString:
		return p.canonicalString(value, p.MinLength, p.MaxLength)
	case KindPath:
		return p.canonicalString(value, 1, maxPathLength)
	case KindInteger:
		return p.canonicalInteger(value)
	case KindBoolean:
		flag, ok := value.(bool)
		if !ok {
			return nil, p.refusal("must be a boolean")
		}
		return encodeJSON(flag)
	case KindDuration:
		return p.canonicalDuration(value)
	case KindEnum:
		text, ok := value.(string)
		if !ok || !slices.Contains(p.enumOn(surface), text) {
			return nil, p.refusal("must be one of its listed values")
		}
		return encodeJSON(text)
	case KindStrings:
		return p.canonicalStrings(value)
	case KindJSON:
		return p.canonicalJSON(value)
	}
	return nil, NewError(ErrorCodeInvalid, "parameter "+p.Name+" has an unknown kind")
}

func (p *Param) canonicalString(value any, minimum, maximum int) ([]byte, error) {
	text, ok := value.(string)
	if !ok {
		return nil, p.refusal("must be a string")
	}
	length := utf8.RuneCountInString(text)
	if length < minimum {
		return nil, p.refusal("is shorter than its minimum length")
	}
	if maximum > 0 && length > maximum {
		return nil, p.refusal("is longer than its maximum length")
	}
	return encodeJSON(text)
}

func (p *Param) canonicalInteger(value any) ([]byte, error) {
	number, ok := value.(json.Number)
	if !ok {
		return nil, p.refusal("must be an integer")
	}
	parsed, err := number.Int64()
	if err != nil {
		return nil, p.refusal("must be an integer")
	}
	if p.Minimum != nil && parsed < *p.Minimum {
		return nil, p.refusal("is below its minimum")
	}
	if p.Maximum != nil && parsed > *p.Maximum {
		return nil, p.refusal("is above its maximum")
	}
	return []byte(strconv.FormatInt(parsed, 10)), nil
}

func (p *Param) canonicalDuration(value any) ([]byte, error) {
	text, ok := value.(string)
	if !ok || !durationRule.MatchString(text) {
		return nil, p.refusal("must be a duration such as 10m")
	}
	if _, err := time.ParseDuration(text); err != nil {
		return nil, p.refusal("must be a duration such as 10m")
	}
	return encodeJSON(text)
}

func (p *Param) canonicalStrings(value any) ([]byte, error) {
	items, ok := value.([]any)
	if !ok || len(items) > maxStringsItems {
		return nil, p.refusal("must be a list of strings within its limit")
	}
	normalized := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		encoded, err := p.canonicalString(item, p.MinLength, p.MaxLength)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, encoded)
	}
	if normalized == nil {
		return []byte("[]"), nil
	}
	return encodeJSON(normalized)
}

func (p *Param) canonicalJSON(value any) ([]byte, error) {
	if len(p.Schema) > 0 {
		wanted, err := schemaType(p.Schema)
		if err != nil {
			return nil, err
		}
		if wanted != "" && !matchesType(wanted, value) {
			return nil, p.refusal("has the wrong JSON type")
		}
	}
	return encodeJSON(value)
}

func schemaType(schema json.RawMessage) (string, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(schema, &head); err != nil {
		return "", wrapError(ErrorCodeInvalid, "a parameter schema is not a JSON object", err)
	}
	return head.Type, nil
}

func matchesType(wanted string, value any) bool {
	switch wanted {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer", "number":
		_, ok := value.(json.Number)
		return ok
	}
	return true
}

func Digest(canonical json.RawMessage) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func kebab(name string) string {
	return strings.ReplaceAll(name, "_", "-")
}
