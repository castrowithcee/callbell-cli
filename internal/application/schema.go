package application

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"unicode/utf8"
)

func validateJSON(schema, raw json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("arguments must be valid JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("arguments must contain exactly one JSON value")
	}
	return validateValue(schema, value)
}

func validateValue(rawSchema json.RawMessage, value any) error {
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		return fmt.Errorf("schema is invalid")
	}
	return validateAt(schema, value, "$")
}

func validateAt(schema map[string]json.RawMessage, value any, path string) error {
	var kind string
	if raw := schema["type"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &kind); err != nil {
			return fmt.Errorf("schema at %s has an invalid type", path)
		}
		if !isType(kind, value) {
			return fmt.Errorf("%s must be %s", path, kind)
		}
	}

	if raw := schema["enum"]; len(raw) > 0 {
		var allowed []any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&allowed) != nil {
			return fmt.Errorf("schema at %s has an invalid enum", path)
		}
		match := false
		encoded, _ := json.Marshal(value)
		for _, candidate := range allowed {
			other, _ := json.Marshal(candidate)
			if bytes.Equal(encoded, other) {
				match = true
				break
			}
		}
		if !match {
			return fmt.Errorf("%s is not an allowed value", path)
		}
	}

	switch value := value.(type) {
	case map[string]any:
		var required []string
		if raw := schema["required"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &required); err != nil {
				return fmt.Errorf("schema at %s has invalid required fields", path)
			}
		}
		for _, name := range required {
			if _, ok := value[name]; !ok {
				return fmt.Errorf("%s.%s is required", path, name)
			}
		}
		properties := map[string]json.RawMessage{}
		if raw := schema["properties"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &properties); err != nil {
				return fmt.Errorf("schema at %s has invalid properties", path)
			}
		}
		additional := true
		if raw := schema["additionalProperties"]; len(raw) > 0 {
			_ = json.Unmarshal(raw, &additional)
		}
		names := make([]string, 0, len(value))
		for name := range value {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child := value[name]
			raw, ok := properties[name]
			if !ok {
				if !additional {
					return fmt.Errorf("%s.%s is not allowed", path, name)
				}
				continue
			}
			var childSchema map[string]json.RawMessage
			if err := json.Unmarshal(raw, &childSchema); err != nil {
				return fmt.Errorf("schema at %s.%s is invalid", path, name)
			}
			if err := validateAt(childSchema, child, path+"."+name); err != nil {
				return err
			}
		}
	case []any:
		if raw := schema["items"]; len(raw) > 0 {
			var itemSchema map[string]json.RawMessage
			if err := json.Unmarshal(raw, &itemSchema); err != nil {
				return fmt.Errorf("schema at %s has invalid items", path)
			}
			for i, item := range value {
				if err := validateAt(itemSchema, item, path+"["+strconv.Itoa(i)+"]"); err != nil {
					return err
				}
			}
		}
	case string:
		if raw := schema["minLength"]; len(raw) > 0 {
			var minimum int
			if json.Unmarshal(raw, &minimum) == nil && utf8.RuneCountInString(value) < minimum {
				return fmt.Errorf("%s is shorter than %d characters", path, minimum)
			}
		}
		if raw := schema["maxLength"]; len(raw) > 0 {
			var maximum int
			if json.Unmarshal(raw, &maximum) == nil && utf8.RuneCountInString(value) > maximum {
				return fmt.Errorf("%s is longer than %d characters", path, maximum)
			}
		}
	case json.Number:
		if raw := schema["minimum"]; len(raw) > 0 {
			minimum, _ := strconv.ParseFloat(string(raw), 64)
			number, _ := value.Float64()
			if number < minimum {
				return fmt.Errorf("%s must be at least %v", path, minimum)
			}
		}
	}
	return nil
}

func isType(kind string, value any) bool {
	switch kind {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := n.Int64()
		return err == nil
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return false
	}
}
