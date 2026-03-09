package api

import (
	"encoding/json"
	"math/big"
	"net/http"
	"reflect"
	"strings"
)

var bigIntType = reflect.TypeOf((*big.Int)(nil))

// writeJSON serializes v to JSON, encoding *big.Int values as strings
// to preserve precision for JavaScript consumers.
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(convertBigInts(reflect.ValueOf(v)))
}

func convertBigInts(v reflect.Value) interface{} {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		if v.Type() == bigIntType {
			return v.Interface().(*big.Int).String()
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Struct:
		return convertStruct(v)
	case reflect.Slice:
		if v.IsNil() {
			return nil
		}
		out := make([]interface{}, v.Len())
		for i := 0; i < v.Len(); i++ {
			out[i] = convertBigInts(v.Index(i))
		}
		return out
	default:
		return v.Interface()
	}
}

func convertStruct(v reflect.Value) map[string]interface{} {
	t := v.Type()
	out := make(map[string]interface{}, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, omit := parseJSONTag(tag)
		if name == "" {
			name = f.Name
		}
		fv := v.Field(i)
		if omit && isEmpty(fv) {
			continue
		}
		out[name] = convertBigInts(fv)
	}
	return out
}

func parseJSONTag(tag string) (name string, omitempty bool) {
	parts := strings.SplitN(tag, ",", 2)
	name = parts[0]
	if len(parts) > 1 {
		omitempty = strings.Contains(parts[1], "omitempty")
	}
	return
}

func isEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Slice, reflect.Map:
		return v.IsNil()
	case reflect.String:
		return v.String() == ""
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Struct:
		return false
	default:
		return false
	}
}
