package tools

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const projectFieldsJSON = `[
	{"$type":"StateProjectCustomField","canBeEmpty":false,"field":{"name":"State","fieldType":{"id":"state[1]","isMultiValue":false,"valueType":"state"}},
	 "bundle":{"values":[{"name":"Open"},{"name":"Fixed"},{"name":"Old","archived":true}]}},
	{"$type":"EnumProjectCustomField","canBeEmpty":true,"emptyFieldText":"No priority","field":{"name":"Priority","fieldType":{"id":"enum[1]","isMultiValue":false,"valueType":"enum"}},
	 "bundle":{"values":[{"name":"Minor"},{"name":"Critical"}]}},
	{"$type":"UserProjectCustomField","canBeEmpty":true,"field":{"name":"Assignee","fieldType":{"id":"user[1]","isMultiValue":false,"valueType":"user"}},"bundle":{}},
	{"$type":"VersionProjectCustomField","canBeEmpty":true,"field":{"name":"Fix versions","fieldType":{"id":"version[*]","isMultiValue":true,"valueType":"version"}},
	 "bundle":{"values":[{"name":"1.0"},{"name":"1.1"}]}},
	{"$type":"PeriodProjectCustomField","canBeEmpty":true,"field":{"name":"Estimation","fieldType":{"id":"period","isMultiValue":false,"valueType":"period"}}},
	{"$type":"SimpleProjectCustomField","canBeEmpty":true,"field":{"name":"Due Date","fieldType":{"id":"date","isMultiValue":false,"valueType":"date"}}},
	{"$type":"SimpleProjectCustomField","canBeEmpty":true,"field":{"name":"Story points","fieldType":{"id":"integer","isMultiValue":false,"valueType":"integer"}}},
	{"$type":"TextProjectCustomField","canBeEmpty":true,"field":{"name":"Notes","fieldType":{"id":"text","isMultiValue":false,"valueType":"text"}}}
]`

func testSchema(t *testing.T) map[string]projectField {
	t.Helper()
	var fields []projectField
	if err := json.Unmarshal([]byte(projectFieldsJSON), &fields); err != nil {
		t.Fatal(err)
	}
	out := map[string]projectField{}
	for _, f := range fields {
		out[f.Field.Name] = f
	}
	return out
}

func TestBuildCustomFields(t *testing.T) {
	got, err := buildCustomFields(testSchema(t), map[string]any{
		"State":        "Fixed",
		"Assignee":     "jdoe",
		"Fix versions": "1.1", // scalar is wrapped for multi-value fields
		"Estimation":   "2h",
		"Due Date":     "2026-01-01",
		"Story points": float64(5),
		"Notes":        "hi",
		"Priority":     nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"name": "Assignee", "$type": "SingleUserIssueCustomField", "value": map[string]any{"login": "jdoe"}},
		{"name": "Due Date", "$type": "DateIssueCustomField", "value": int64(1767225600000)},
		{"name": "Estimation", "$type": "PeriodIssueCustomField", "value": map[string]any{"$type": "PeriodValue", "presentation": "2h"}},
		{"name": "Fix versions", "$type": "MultiVersionIssueCustomField", "value": []any{map[string]any{"name": "1.1"}}},
		{"name": "Notes", "$type": "TextIssueCustomField", "value": map[string]any{"$type": "TextFieldValue", "text": "hi"}},
		{"name": "Priority", "$type": "SingleEnumIssueCustomField", "value": nil},
		{"name": "State", "$type": "StateIssueCustomField", "value": map[string]any{"name": "Fixed"}},
		{"name": "Story points", "$type": "SimpleIssueCustomField", "value": int64(5)},
	}
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.MarshalIndent(got, "", " ")
		t.Errorf("got %s", gj)
	}
}

func TestBuildCustomFieldsErrors(t *testing.T) {
	schema := testSchema(t)
	for name, values := range map[string]map[string]any{
		"unknown field":  {"Severity": "High"},
		"archived value": {"State": "Old"},
		"bad value":      {"Priority": "Urgent"},
		"bad date":       {"Due Date": "tomorrow"},
		"two for single": {"State": []any{"Open", "Fixed"}},
	} {
		if _, err := buildCustomFields(schema, values); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestFieldsJSONSchema(t *testing.T) {
	s := fieldsJSONSchema(testSchema(t))
	if got := s["required"]; !reflect.DeepEqual(got, []string{"State"}) {
		t.Errorf("required = %v", got)
	}
	props := s["properties"].(map[string]any)
	state := props["State"].(map[string]any)
	if !reflect.DeepEqual(state["enum"], []string{"Open", "Fixed"}) {
		t.Errorf("State enum = %v", state["enum"])
	}
	fix := props["Fix versions"].(map[string]any)
	if fix["type"] != "array" {
		t.Errorf("Fix versions type = %v", fix["type"])
	}
	if d := props["Priority"].(map[string]any)["description"].(string); !strings.Contains(d, "No priority") {
		t.Errorf("Priority description = %q", d)
	}
}
