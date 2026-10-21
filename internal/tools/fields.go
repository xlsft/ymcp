package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// projectField is a custom field attached to a project.
type projectField struct {
	Type           string `json:"$type"`
	CanBeEmpty     bool   `json:"canBeEmpty"`
	EmptyFieldText string `json:"emptyFieldText"`
	Field          struct {
		Name      string `json:"name"`
		FieldType struct {
			ID           string `json:"id"`
			IsMultiValue bool   `json:"isMultiValue"`
			ValueType    string `json:"valueType"`
		} `json:"fieldType"`
	} `json:"field"`
	Bundle *struct {
		Values []struct {
			Name     string `json:"name"`
			Archived bool   `json:"archived"`
		} `json:"values"`
	} `json:"bundle"`
}

const projectFieldFields = "$type,canBeEmpty,emptyFieldText," +
	"field(name,fieldType(id,isMultiValue,valueType))," +
	"bundle(values(name,archived))"

func (f projectField) multi() bool { return f.Field.FieldType.IsMultiValue }

// kind classifies the field by the shape of value it takes.
func (f projectField) kind() string {
	switch f.Type {
	case "UserProjectCustomField":
		return "user"
	case "PeriodProjectCustomField":
		return "period"
	case "TextProjectCustomField":
		return "text"
	case "SimpleProjectCustomField":
		switch f.Field.FieldType.ValueType {
		case "date", "date and time":
			return "date"
		case "integer":
			return "integer"
		case "float":
			return "float"
		default:
			return "string"
		}
	default:
		// Enum, State, Version, Build, Owned and Group fields take a bundle element name.
		return "bundle"
	}
}

func (f projectField) values() []string {
	if f.Bundle == nil {
		return nil
	}
	out := make([]string, 0, len(f.Bundle.Values))
	for _, v := range f.Bundle.Values {
		if !v.Archived {
			out = append(out, v.Name)
		}
	}
	return out
}

// issueFieldType maps the project field to the $type of the issue field.
func (f projectField) issueFieldType() string {
	base := strings.TrimSuffix(f.Type, "ProjectCustomField")
	switch base {
	case "State", "Period", "Text":
		return base + "IssueCustomField"
	case "Simple":
		if f.kind() == "date" {
			return "DateIssueCustomField"
		}
		return "SimpleIssueCustomField"
	}
	if f.multi() {
		return "Multi" + base + "IssueCustomField"
	}
	return "Single" + base + "IssueCustomField"
}

// projectFields returns the custom fields of a project keyed by name.
func (t *toolset) projectFields(ctx context.Context, projectID string) (map[string]projectField, error) {
	var fields []projectField
	q := url.Values{"fields": {projectFieldFields}, "$top": {"-1"}}
	if err := t.yt.Get(ctx, "admin/projects/"+url.PathEscape(projectID)+"/customFields", q, &fields); err != nil {
		return nil, err
	}
	out := make(map[string]projectField, len(fields))
	for _, f := range fields {
		out[f.Field.Name] = f
	}
	return out, nil
}

// buildCustomFields converts {"Field name": value} into the REST payload.
func buildCustomFields(schema map[string]projectField, values map[string]any) ([]map[string]any, error) {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]map[string]any, 0, len(values))
	for _, name := range names {
		f, ok := schema[name]
		if !ok {
			return nil, fmt.Errorf("unknown custom field %q; available: %s", name, strings.Join(fieldNames(schema), ", "))
		}
		v, err := f.encode(values[name])
		if err != nil {
			return nil, fmt.Errorf("custom field %q: %w", name, err)
		}
		out = append(out, map[string]any{"name": name, "$type": f.issueFieldType(), "value": v})
	}
	return out, nil
}

func fieldNames(schema map[string]projectField) []string {
	names := make([]string, 0, len(schema))
	for name := range schema {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// encode converts a plain input value into the REST value for this field.
func (f projectField) encode(v any) (any, error) {
	if v == nil {
		if f.multi() {
			return []any{}, nil
		}
		return nil, nil
	}
	if f.multi() {
		items, ok := v.([]any)
		if !ok {
			items = []any{v}
		}
		out := make([]any, 0, len(items))
		for _, item := range items {
			e, err := f.encodeOne(item)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	}
	if items, ok := v.([]any); ok {
		if len(items) != 1 {
			return nil, fmt.Errorf("single-value field takes one value, got %d", len(items))
		}
		v = items[0]
	}
	return f.encodeOne(v)
}

func (f projectField) encodeOne(v any) (any, error) {
	switch f.kind() {
	case "integer":
		n, err := toNumber(v)
		return int64(n), err
	case "float":
		return toNumber(v)
	case "date":
		return toMillis(v)
	}

	s, ok := v.(string)
	if !ok {
		if n, isNum := v.(float64); isNum {
			s = strconv.FormatFloat(n, 'f', -1, 64)
		} else {
			return nil, fmt.Errorf("expected a string, got %T", v)
		}
	}
	switch f.kind() {
	case "user":
		return map[string]any{"login": s}, nil
	case "period":
		return map[string]any{"$type": "PeriodValue", "presentation": s}, nil
	case "text":
		return map[string]any{"$type": "TextFieldValue", "text": s}, nil
	case "string":
		return s, nil
	}
	if vals := f.values(); len(vals) > 0 && !slices.Contains(vals, s) {
		return nil, fmt.Errorf("value %q is not allowed; allowed: %s", s, strings.Join(vals, ", "))
	}
	return map[string]any{"name": s}, nil
}

func toNumber(v any) (float64, error) {
	switch v := v.(type) {
	case float64:
		return v, nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(v), 64)
	case json.Number:
		return v.Float64()
	}
	return 0, fmt.Errorf("expected a number, got %T", v)
}

func toMillis(v any) (int64, error) {
	switch v := v.(type) {
	case float64:
		return int64(v), nil
	case string:
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t.UnixMilli(), nil
			}
		}
		return 0, fmt.Errorf("cannot parse date %q, use yyyy-MM-dd or RFC 3339", v)
	}
	return 0, fmt.Errorf("expected a date string, got %T", v)
}

// jsonSchema describes the project fields the way get_issue_fields_schema does.
func fieldsJSONSchema(schema map[string]projectField) map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, name := range fieldNames(schema) {
		f := schema[name]
		item := map[string]any{}
		desc := ""
		switch f.kind() {
		case "user":
			item["type"], desc = "string", "User login"
		case "period":
			item["type"], desc = "string", "Period, e.g. 1w 2d 4h 30m"
		case "text":
			item["type"], desc = "string", "Text, Markdown supported"
		case "date":
			item["type"], desc = "string", "Date, yyyy-MM-dd or RFC 3339"
		case "integer":
			item["type"] = "integer"
		case "float":
			item["type"] = "number"
		case "string":
			item["type"] = "string"
		default:
			item["type"] = "string"
			if vals := f.values(); len(vals) > 0 {
				item["enum"] = vals
			}
		}
		var prop map[string]any
		if f.multi() {
			prop = map[string]any{"type": "array", "items": item}
		} else {
			prop = item
		}
		if f.EmptyFieldText != "" {
			desc = strings.TrimSpace(desc + ". Empty value: " + f.EmptyFieldText)
			desc = strings.TrimPrefix(desc, ". ")
		}
		if desc != "" {
			prop["description"] = desc
		}
		props[name] = prop
		if !f.CanBeEmpty {
			required = append(required, name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}
