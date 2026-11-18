package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/jmespath/go-jmespath"
)

// numberPrecision is the precision of Terraform numbers (cty uses 512 bits).
const numberPrecision = 512

// maxExactInt is the largest integer a float64 holds exactly (2^53).
const maxExactInt = 1 << 53

var errUnknown = errors.New("the value is not known yet")

// toJSON converts a Terraform value, such as the HCL value of body, to the
// decoded JSON form that decodeJSON produces: objects and maps become
// map[string]any, lists, sets and tuples []any, numbers json.Number, null nil.
func toJSON(ctx context.Context, v attr.Value) (any, error) {
	if v == nil || v.IsNull() {
		return nil, nil
	}
	if v.IsUnknown() {
		return nil, errUnknown
	}
	switch t := v.(type) {
	case basetypes.DynamicValue:
		return toJSON(ctx, t.UnderlyingValue())
	case basetypes.ObjectValue:
		return objectToJSON(ctx, t.Attributes())
	case basetypes.MapValue:
		return objectToJSON(ctx, t.Elements())
	case basetypes.ListValue:
		return listToJSON(ctx, t.Elements())
	case basetypes.SetValue:
		return listToJSON(ctx, t.Elements())
	case basetypes.TupleValue:
		return listToJSON(ctx, t.Elements())
	case basetypes.StringValue:
		return t.ValueString(), nil
	case basetypes.BoolValue:
		return t.ValueBool(), nil
	case basetypes.NumberValue:
		return formatNumber(t.ValueBigFloat())
	case basetypes.Int64Value:
		return json.Number(strconv.FormatInt(t.ValueInt64(), 10)), nil
	case basetypes.Float64Value:
		return formatNumber(big.NewFloat(t.ValueFloat64()))
	default:
		return nil, fmt.Errorf("values of type %s cannot be sent as JSON", v.Type(ctx))
	}
}

func objectToJSON(ctx context.Context, attrs map[string]attr.Value) (any, error) {
	out := make(map[string]any, len(attrs))
	for k, v := range attrs {
		j, err := toJSON(ctx, v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = j
	}
	return out, nil
}

func listToJSON(ctx context.Context, elems []attr.Value) (any, error) {
	out := make([]any, len(elems))
	for i, v := range elems {
		j, err := toJSON(ctx, v)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out[i] = j
	}
	return out, nil
}

// formatNumber writes integers without an exponent (1000000, not 1e+06), since
// some APIs reject the exponent form for integer properties.
func formatNumber(f *big.Float) (any, error) {
	if f.IsInf() {
		return nil, errors.New("an infinite number cannot be sent as JSON")
	}
	if f.IsInt() {
		return json.Number(f.Text('f', 0)), nil
	}
	return json.Number(f.Text('g', -1)), nil
}

// fromJSON converts a decoded JSON value to a Terraform value with the types the
// JSON implies: objects become objects, arrays tuples, numbers numbers, and null
// a null of the dynamic type.
func fromJSON(ctx context.Context, v any) (attr.Value, error) {
	switch t := v.(type) {
	case nil:
		return types.DynamicNull(), nil
	case bool:
		return types.BoolValue(t), nil
	case string:
		return types.StringValue(t), nil
	case json.Number:
		return parseNumber(string(t))
	case float64:
		// go-jmespath computes in float64. Its shortest decimal form keeps 0.1 as
		// 0.1 instead of the binary fraction float64 actually holds.
		return parseNumber(strconv.FormatFloat(t, 'g', -1, 64))
	case map[string]any:
		vals := make(map[string]attr.Value, len(t))
		typs := make(map[string]attr.Type, len(t))
		for k, e := range t {
			ev, err := fromJSON(ctx, e)
			if err != nil {
				return nil, err
			}
			vals[k], typs[k] = ev, ev.Type(ctx)
		}
		ov, d := types.ObjectValue(typs, vals)
		if d.HasError() {
			return nil, diagsError(d)
		}
		return ov, nil
	case []any:
		vals := make([]attr.Value, len(t))
		typs := make([]attr.Type, len(t))
		for i, e := range t {
			ev, err := fromJSON(ctx, e)
			if err != nil {
				return nil, err
			}
			vals[i], typs[i] = ev, ev.Type(ctx)
		}
		tv, d := types.TupleValue(typs, vals)
		if d.HasError() {
			return nil, diagsError(d)
		}
		return tv, nil
	default:
		return nil, fmt.Errorf("unsupported JSON value of type %T", v)
	}
}

// fromJSONDynamic is fromJSON for a dynamic attribute.
func fromJSONDynamic(ctx context.Context, v any) (types.Dynamic, error) {
	av, err := fromJSON(ctx, v)
	if err != nil {
		return types.DynamicNull(), err
	}
	if d, ok := av.(basetypes.DynamicValue); ok {
		return d, nil
	}
	return types.DynamicValue(av), nil
}

func parseNumber(s string) (attr.Value, error) {
	f, _, err := big.ParseFloat(s, 10, numberPrecision, big.ToNearestEven)
	if err != nil {
		return nil, fmt.Errorf("number %s: %w", s, err)
	}
	return types.NumberValue(f), nil
}

func diagsError(d diag.Diagnostics) error {
	msgs := make([]string, 0, len(d.Errors()))
	for _, e := range d.Errors() {
		msgs = append(msgs, e.Summary()+": "+e.Detail())
	}
	return errors.New(strings.Join(msgs, "; "))
}

// exportValues builds output from a response, like the msgraph provider: one
// attribute per entry of response_export_values, holding the result of its
// JMESPath query ("@" is the whole response). Without response_export_values,
// output is an empty object. A query that fails on this response is left out
// with a warning, so a refresh does not fail because of an export.
func exportValues(ctx context.Context, response any, queries map[string]string) (types.Dynamic, diag.Diagnostics) {
	var diags diag.Diagnostics
	out := make(map[string]any, len(queries))
	if len(queries) > 0 {
		data := jmesInput(response)
		names := make([]string, 0, len(queries))
		for name := range queries {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			v, err := jmespath.Search(queries[name], data)
			if err != nil {
				diags.AddWarning("Cannot export a response value",
					fmt.Sprintf("response_export_values[%q] (%s) failed on the response and is left out of output: %s", name, queries[name], err))
				continue
			}
			out[name] = v
		}
	}
	v, err := fromJSONDynamic(ctx, out)
	if err != nil {
		diags.AddError("Converting the response failed", err.Error())
	}
	return v, diags
}

// jmesInput converts the numbers of a decoded response to float64, which is what
// go-jmespath compares and computes with. An integer that float64 cannot hold
// exactly (a large numeric ID) stays a json.Number, so it is exported unchanged.
func jmesInput(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = jmesInput(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = jmesInput(e)
		}
		return out
	case json.Number:
		if !strings.ContainsAny(string(t), ".eE") {
			if i, err := t.Int64(); err != nil || i > maxExactInt || i < -maxExactInt {
				return t
			}
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t
	default:
		return v
	}
}
