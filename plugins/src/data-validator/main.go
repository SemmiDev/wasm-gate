// Plugin: data-validator (WASI-native)
//
// Menerima JSON dengan field "schema" dan "data", memvalidasi kesesuaiannya.
//
// Input:  { "schema": {"name":"string","age":"number"}, "data": {"name":"Alice","age":30} }
// Output: { "valid": true, "errors": [], "checked_fields": 2 }
//
// Build:
//   GOOS=wasip1 GOARCH=wasm go build -o data-validator.wasm .
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

type Request struct {
	Schema map[string]string      `json:"schema"`
	Data   map[string]interface{} `json:"data"`
}

func main() {
	var req Request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		writeOutput(map[string]interface{}{"error": "invalid input: " + err.Error()})
		os.Exit(1)
	}

	var errs []string
	for field, expectedType := range req.Schema {
		val, exists := req.Data[field]
		if !exists {
			errs = append(errs, fmt.Sprintf("missing required field %q", field))
			continue
		}
		if err := checkType(field, val, expectedType); err != nil {
			errs = append(errs, err.Error())
		}
	}

	writeOutput(map[string]interface{}{
		"valid":          len(errs) == 0,
		"errors":         errs,
		"checked_fields": len(req.Schema),
		"data_fields":    len(req.Data),
		"_plugin":        "data-validator@1.0.0",
	})
}

func checkType(field string, val interface{}, want string) error {
	ok := false
	switch want {
	case "string":
		_, ok = val.(string)
	case "number":
		_, ok = val.(float64)
	case "bool":
		_, ok = val.(bool)
	case "array":
		_, ok = val.([]interface{})
	case "object":
		_, ok = val.(map[string]interface{})
	default:
		return nil // unknown types pass-through
	}
	if !ok {
		return fmt.Errorf("field %q: expected %s, got %T", field, want, val)
	}
	return nil
}

func writeOutput(v interface{}) {
	json.NewEncoder(os.Stdout).Encode(v)
}
