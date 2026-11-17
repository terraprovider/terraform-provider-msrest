package provider

import (
	"strings"
	"testing"
)

func mustDecode(t *testing.T, s string) any {
	t.Helper()
	v, err := decodeJSON([]byte(s))
	if err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return v
}

func TestDecodeJSONRejectsTrailingData(t *testing.T) {
	if _, err := decodeJSON([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Fatal("expected an error for trailing data")
	}
	if _, err := decodeJSON([]byte(`{"a":`)); err == nil {
		t.Fatal("expected an error for truncated JSON")
	}
}

func TestJSONEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{`{"a":1,"b":[true,null,"x"]}`, `{"b":[true,null,"x"],"a":1.0}`, true},
		{`{"a":1}`, `{"a":"1"}`, false},
		{`{"a":1}`, `{"a":1,"b":2}`, false},
		{`[1,2]`, `[2,1]`, false},
		{`12345678901234567890`, `12345678901234567890.0`, true},
		{`12345678901234567890`, `12345678901234567891`, false},
		{`null`, `null`, true},
	}
	for _, c := range cases {
		if got := jsonEqual(mustDecode(t, c.a), mustDecode(t, c.b)); got != c.want {
			t.Errorf("jsonEqual(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestProject(t *testing.T) {
	actual := `{
		"@odata.context": "https://tasks.office.com/taskapi/tenantAdminSettings/$metadata#Settings",
		"id": "1",
		"allowCalendarSharing": true,
		"isPlannerAllowed": true,
		"Nested": {"a": 1, "b": 2},
		"list": [{"k": "x", "extra": 1}, {"k": "y", "extra": 2}],
		"other": [1, 2, 3]
	}`
	cases := []struct {
		name, desired, want string
	}{
		{"subset of scalars", `{"allowCalendarSharing":false}`, `{"allowCalendarSharing":true}`},
		{"case-insensitive key keeps the desired spelling", `{"nested":{"a":5}}`, `{"nested":{"a":1}}`},
		{"missing key keeps the desired value", `{"writeOnly":"x","isPlannerAllowed":false}`, `{"isPlannerAllowed":true,"writeOnly":"x"}`},
		{"arrays of equal length are projected", `{"list":[{"k":"x"},{"k":"z"}]}`, `{"list":[{"k":"x"},{"k":"y"}]}`},
		{"arrays of other length are returned as is", `{"other":[1]}`, `{"other":[1,2,3]}`},
		{"type mismatch returns actual", `{"Nested":"flat"}`, `{"Nested":{"a":1,"b":2}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := project(mustDecode(t, c.desired), mustDecode(t, actual))
			if !jsonEqual(got, mustDecode(t, c.want)) {
				s, _ := encodeJSON(got)
				t.Errorf("project = %s, want %s", s, c.want)
			}
		})
	}
}

func TestReflects(t *testing.T) {
	actual := mustDecode(t, `{"policyValue":"Disabled","productId":"CFQ7TTC0LH3L","productName":"Power Automate per user"}`)
	if !reflects(mustDecode(t, `{"policyValue":"Disabled"}`), actual) {
		t.Error("expected the response to reflect policyValue=Disabled")
	}
	if reflects(mustDecode(t, `{"policyValue":"Enabled"}`), actual) {
		t.Error("expected the response not to reflect policyValue=Enabled")
	}
}

func TestEncodeJSONIsCanonical(t *testing.T) {
	s, err := encodeJSON(mustDecode(t, `{"b":"<x>","a":12345678901234567890}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":12345678901234567890,"b":"<x>"}`; s != want {
		t.Errorf("encodeJSON = %s, want %s", s, want)
	}
}

func TestTokenResource(t *testing.T) {
	cases := []struct{ scope, url, want string }{
		{"", "https://tasks.office.com/taskAPI/tenantAdminSettings/Settings", "https://tasks.office.com"},
		{"https://tasks.office.com/.default", "https://example.com/x", "https://tasks.office.com"},
		{"aeb86249-8ea3-49e2-900b-54cc8e308f85/.default", "https://licensing.m365.microsoft.com/v1/x", "aeb86249-8ea3-49e2-900b-54cc8e308f85"},
		{"aeb86249-8ea3-49e2-900b-54cc8e308f85", "https://licensing.m365.microsoft.com/v1/x", "aeb86249-8ea3-49e2-900b-54cc8e308f85"},
	}
	for _, c := range cases {
		got, err := tokenResource(c.scope, c.url)
		if err != nil || got != c.want {
			t.Errorf("tokenResource(%q, %q) = %q, %v; want %q", c.scope, c.url, got, err, c.want)
		}
	}
	if _, err := tokenResource("", "/relative"); err == nil {
		t.Error("expected an error for a relative URL without scope")
	}
}

func TestMissingPaths(t *testing.T) {
	actual := mustDecode(t, `{"allowCalendarSharing":true,"Nested":{"a":1},"list":[{"k":"x"}]}`)
	desired := mustDecode(t, `{"alowCalendarSharing":false,"allowCalendarSharing":false,"nested":{"a":2,"b":3},"list":[{"k":"y","z":1}],"other":[1,2]}`)
	got := strings.Join(missingPaths(desired, actual), ",")
	if want := "alowCalendarSharing,list[0].z,nested.b,other"; got != want {
		t.Errorf("missingPaths = %s, want %s", got, want)
	}
	if got := missingPaths(mustDecode(t, `{"a":1}`), mustDecode(t, `{"a":2}`)); len(got) != 0 {
		t.Errorf("missingPaths = %v, want none", got)
	}
}
