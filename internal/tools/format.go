package tools

import (
	"encoding/json"
	"strings"
	"time"
)

// customField is an issue custom field as returned by the REST API.
type customField struct {
	Name  string          `json:"name"`
	Type  string          `json:"$type"`
	Value json.RawMessage `json:"value"`
}

// customFieldValueFields is the ?fields= projection that covers every
// custom field value type.
const customFieldValueFields = "name,$type,value(name,login,fullName,presentation,text,minutes)"

// flattenFields turns custom fields into {"State": "Open", "Assignee": "jdoe"}.
func flattenFields(fields []customField) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		out[f.Name] = fieldValue(f.Type, f.Value)
	}
	return out
}

// fieldValue reduces a custom field value to the plain form tools accept
// as input: names for bundle values, logins for users, text for text
// fields, presentation for periods, timestamps for dates.
func fieldValue(fieldType string, raw json.RawMessage) any {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v == nil {
		return nil
	}
	return plainValue(fieldType, v)
}

func plainValue(fieldType string, v any) any {
	switch v := v.(type) {
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, plainValue(fieldType, item))
		}
		return out
	case map[string]any:
		for _, key := range []string{"login", "name", "presentation", "text", "fullName"} {
			if s, ok := v[key].(string); ok && s != "" {
				return s
			}
		}
		return v
	case float64:
		if strings.HasPrefix(fieldType, "Date") {
			return formatMillis(int64(v))
		}
		return v
	default:
		return v
	}
}

// formatMillis renders a YouTrack timestamp (ms since epoch) as RFC 3339.
func formatMillis(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// millis is a YouTrack timestamp that marshals as RFC 3339.
type millis int64

func (m millis) MarshalJSON() ([]byte, error) {
	if m == 0 {
		return []byte("null"), nil
	}
	return json.Marshal(formatMillis(int64(m)))
}

// userRef is the compact user shape used inside other entities.
type userRef struct {
	Login    string `json:"login"`
	FullName string `json:"fullName,omitempty"`
}

// page clamps offset/limit to the bounds the official tools use.
func page(offset, limit, def, max int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = def
	}
	if limit > max {
		limit = max
	}
	return offset, limit
}
