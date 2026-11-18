package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestJSONRoundTrip(t *testing.T) {
	ctx := context.Background()
	doc := `{"a":1,"b":0.1,"c":12345678901234567890123,"d":[true,null,"x",{"e":[]}],"f":{},"g":null,"h":-2.5e-7}`
	v, err := fromJSON(ctx, mustDecode(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	back, err := toJSON(ctx, types.DynamicValue(v))
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(back, mustDecode(t, doc)) {
		s, _ := encodeJSON(back)
		t.Errorf("round trip = %s, want %s", s, doc)
	}
}

func TestToJSONCollectionTypes(t *testing.T) {
	ctx := context.Background()
	// Values as they reach the provider from typed variables rather than literals.
	obj := types.ObjectValueMust(
		map[string]attr.Type{
			"list": types.ListType{ElemType: types.StringType},
			"set":  types.SetType{ElemType: types.NumberType},
			"map":  types.MapType{ElemType: types.BoolType},
			"int":  types.Int64Type,
			"none": types.StringType,
		},
		map[string]attr.Value{
			"list": types.ListValueMust(types.StringType, []attr.Value{types.StringValue("x")}),
			"set":  types.SetValueMust(types.NumberType, []attr.Value{types.NumberValue(big.NewFloat(1e6))}),
			"map":  types.MapValueMust(types.BoolType, map[string]attr.Value{"k": types.BoolValue(true)}),
			"int":  types.Int64Value(42),
			"none": types.StringNull(),
		},
	)
	got, err := toJSON(ctx, types.DynamicValue(obj))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := encodeJSON(got)
	if want := `{"int":42,"list":["x"],"map":{"k":true},"none":null,"set":[1000000]}`; s != want {
		t.Errorf("toJSON = %s, want %s", s, want)
	}
}

func TestToJSONUnknown(t *testing.T) {
	obj := types.ObjectValueMust(map[string]attr.Type{"a": types.BoolType}, map[string]attr.Value{"a": types.BoolUnknown()})
	if _, err := toJSON(context.Background(), obj); !errors.Is(err, errUnknown) {
		t.Errorf("toJSON of an unknown value: err = %v, want errUnknown", err)
	}
}

func TestFormatNumber(t *testing.T) {
	cases := map[string]string{
		"1000000":                 "1000000",
		"2.5":                     "2.5",
		"0.1":                     "0.1",
		"-3":                      "-3",
		"12345678901234567890123": "12345678901234567890123",
	}
	for in, want := range cases {
		f, _, _ := big.ParseFloat(in, 10, numberPrecision, big.ToNearestEven)
		got, err := formatNumber(f)
		if err != nil || got != json.Number(want) {
			t.Errorf("formatNumber(%s) = %v, %v; want %s", in, got, err, want)
		}
	}
}

func TestJMESInputKeepsLargeIntegers(t *testing.T) {
	v := jmesInput(mustDecode(t, `{"small":42,"dec":0.5,"big":9007199254740993,"huge":123456789012345678901234567890}`)).(map[string]any)
	if v["small"] != 42.0 || v["dec"] != 0.5 {
		t.Errorf("small numbers should become float64: %#v", v)
	}
	if v["big"] != json.Number("9007199254740993") || v["huge"] != json.Number("123456789012345678901234567890") {
		t.Errorf("large integers should stay json.Number: %#v", v)
	}
}

func TestExportValues(t *testing.T) {
	ctx := context.Background()
	response := mustDecode(t, `{"items":[{"productId":"P1","policyValue":"Enabled","n":1},{"productId":"P2","policyValue":"Disabled","n":2}],"id":9007199254740993}`)

	out, diags := exportValues(ctx, response, nil)
	if diags.HasError() {
		t.Fatal(diags)
	}
	if got, _ := toJSON(ctx, out); !jsonEqual(got, map[string]any{}) {
		t.Errorf("no queries: output = %v, want {}", got)
	}

	out, diags = exportValues(ctx, response, map[string]string{
		"all":      "@",
		"ids":      "items[].productId",
		"disabled": "items[?policyValue=='Disabled'].productId",
		"total":    "sum(items[].n)",
		"id":       "id",
		"broken":   "abs(items[0].productId)",
	})
	if diags.HasError() {
		t.Fatal(diags)
	}
	if diags.WarningsCount() != 1 {
		t.Errorf("want one warning for the failing query, got %v", diags)
	}
	got, err := toJSON(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	want := mustDecode(t, `{"ids":["P1","P2"],"disabled":["P2"],"total":3,"id":9007199254740993}`).(map[string]any)
	want["all"] = response
	if !jsonEqual(got, want) {
		s, _ := encodeJSON(got)
		t.Errorf("output = %s", s)
	}
}
