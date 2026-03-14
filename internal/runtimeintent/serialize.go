package runtimeintent

import (
	"encoding/json"
	"sort"
)

// Serialize produces deterministic JSON (sorted keys) for hashability.
func Serialize(intent *RuntimeIntent) ([]byte, error) {
	data, err := json.Marshal(intent)
	if err != nil {
		return nil, err
	}
	// Use a map to get sorted keys
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return json.Marshal(sortMapKeys(m))
}

func sortMapKeys(m map[string]interface{}) map[string]interface{} {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	result := make(map[string]interface{}, len(m))
	for _, k := range keys {
		v := m[k]
		if vm, ok := v.(map[string]interface{}); ok {
			v = sortMapKeys(vm)
		}
		result[k] = v
	}
	return result
}
