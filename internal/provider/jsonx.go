package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
)

// decodeJSON parses one JSON value, keeping numbers as json.Number so large
// integers survive a round trip unchanged.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return v, nil
}

// encodeJSON returns the compact JSON form of v with object keys sorted and
// without HTML escaping.
func encodeJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// jsonEqual reports whether two decoded JSON values are semantically equal:
// key order and number formatting (1 vs 1.0) do not matter.
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !jsonEqual(xv, yv) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		if x == y {
			return true
		}
		xf, _, errX := big.ParseFloat(string(x), 10, 256, big.ToNearestEven)
		yf, _, errY := big.ParseFloat(string(y), 10, 256, big.ToNearestEven)
		return errX == nil && errY == nil && xf.Cmp(yf) == 0
	default:
		// string, bool, nil
		return a == b
	}
}

// project reduces actual (an API response) to the shape of desired (the
// configured body), so that properties the configuration does not manage are
// ignored when detecting drift:
//
//   - objects: for every key of desired, the value of the same key in actual
//     (matched exactly, else case-insensitively), projected recursively. A key
//     that actual does not return keeps its desired value, because the API
//     treats it as write-only and there is nothing to compare;
//   - arrays of the same length are projected element by element; otherwise
//     the actual array is returned unchanged;
//   - everything else: the actual value.
func project(desired, actual any) any {
	switch d := desired.(type) {
	case map[string]any:
		a, ok := actual.(map[string]any)
		if !ok {
			return actual
		}
		out := make(map[string]any, len(d))
		for k, dv := range d {
			av, found := lookupKey(a, k)
			if !found {
				out[k] = dv
				continue
			}
			out[k] = project(dv, av)
		}
		return out
	case []any:
		a, ok := actual.([]any)
		if !ok || len(a) != len(d) {
			return actual
		}
		out := make([]any, len(d))
		for i := range d {
			out[i] = project(d[i], a[i])
		}
		return out
	default:
		return actual
	}
}

func lookupKey(m map[string]any, key string) (any, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

// missingPaths lists the properties of desired that actual does not return, as
// paths such as "a.b" or "list[0].c", sorted. project treats them as write-only,
// so a misspelled property name would otherwise go unnoticed.
func missingPaths(desired, actual any) []string {
	var out []string
	var walk func(d, a any, prefix string)
	walk = func(d, a any, prefix string) {
		switch dv := d.(type) {
		case map[string]any:
			am, ok := a.(map[string]any)
			if !ok {
				return
			}
			for k, v := range dv {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				av, found := lookupKey(am, k)
				if !found {
					out = append(out, p)
					continue
				}
				walk(v, av, p)
			}
		case []any:
			aa, ok := a.([]any)
			if !ok || len(aa) != len(dv) {
				return
			}
			for i := range dv {
				walk(dv[i], aa[i], fmt.Sprintf("%s[%d]", prefix, i))
			}
		}
	}
	walk(desired, actual, "")
	sort.Strings(out)
	return out
}

// reflects reports whether actual already contains every value of desired.
func reflects(desired, actual any) bool {
	return jsonEqual(project(desired, actual), desired)
}
