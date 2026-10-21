package tools

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestFlattenFields(t *testing.T) {
	var fields []customField
	err := json.Unmarshal([]byte(`[
		{"name":"State","$type":"StateIssueCustomField","value":{"name":"Open","$type":"StateBundleElement"}},
		{"name":"Assignee","$type":"SingleUserIssueCustomField","value":{"login":"jdoe","fullName":"John Doe"}},
		{"name":"Fix versions","$type":"MultiVersionIssueCustomField","value":[{"name":"1.0"},{"name":"1.1"}]},
		{"name":"Estimation","$type":"PeriodIssueCustomField","value":{"minutes":90,"presentation":"1h 30m"}},
		{"name":"Notes","$type":"TextIssueCustomField","value":{"text":"hello"}},
		{"name":"Due Date","$type":"DateIssueCustomField","value":1767225600000},
		{"name":"Story points","$type":"SimpleIssueCustomField","value":3},
		{"name":"Priority","$type":"SingleEnumIssueCustomField","value":null}
	]`), &fields)
	if err != nil {
		t.Fatal(err)
	}

	got := flattenFields(fields)
	want := map[string]any{
		"State":        "Open",
		"Assignee":     "jdoe",
		"Fix versions": []any{"1.0", "1.1"},
		"Estimation":   "1h 30m",
		"Notes":        "hello",
		"Due Date":     "2026-01-01T00:00:00Z",
		"Story points": float64(3),
		"Priority":     nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flattenFields =\n%#v\nwant\n%#v", got, want)
	}
}

func TestPage(t *testing.T) {
	for _, c := range []struct{ off, lim, wantOff, wantLim int }{
		{0, 0, 0, 20},
		{-5, 50, 0, 20},
		{10, 5, 10, 5},
	} {
		off, lim := page(c.off, c.lim, 20, 20)
		if off != c.wantOff || lim != c.wantLim {
			t.Errorf("page(%d,%d) = %d,%d", c.off, c.lim, off, lim)
		}
	}
}
