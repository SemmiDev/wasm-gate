// Plugin: json-transformer (WASI-native)
//
// Menerima JSON object dari stdin, mengembalikan versi yang ditransformasi ke stdout.
// - string values → UPPERCASE
// - number values → dikali 2
// - bool values → di-flip
//
// Build:
//   GOOS=wasip1 GOARCH=wasm go build -o json-transformer.wasm .
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
	// Baca seluruh input dari stdin (di-inject oleh WasmGate host)
	var input map[string]interface{}
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		writeError("invalid JSON input: " + err.Error())
		os.Exit(1)
	}

	result := make(map[string]interface{})
	for k, v := range input {
		switch val := v.(type) {
		case string:
			result[k] = strings.ToUpper(val)
		case float64:
			result[k] = val * 2
		case bool:
			result[k] = !val
		default:
			result[k] = v
		}
	}
	result["_plugin"] = "json-transformer@1.0.0"
	result["_fields_transformed"] = len(input)

	// Tulis output ke stdout — host akan membaca ini sebagai hasil invocation
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		writeError("encoding output: " + err.Error())
		os.Exit(1)
	}
}

func writeError(msg string) {
	fmt.Fprintf(os.Stdout, `{"error":%q}`, msg)
}
