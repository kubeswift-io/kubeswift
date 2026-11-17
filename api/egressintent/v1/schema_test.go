// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

//go:embed egress-intent.schema.json
var schemaJSON []byte

//go:embed testdata/example.json
var exampleJSON []byte

type schemaObject struct {
	Required   []string                   `json:"required"`
	Properties map[string]json.RawMessage `json:"properties"`
	Defs       map[string]schemaObject    `json:"$defs"`
}

// jsonFields returns a struct's JSON property names and the required ones
// (those without omitempty).
func jsonFields(t reflect.Type) (all, required []string) {
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		all = append(all, name)
		if !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
	}
	sort.Strings(all)
	sort.Strings(required)
	return all, required
}

func keys(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// The Go types and the published schema describe the same properties, and
// agree on which are required.
func TestSchemaMatchesTypes(t *testing.T) {
	var root schemaObject
	if err := json.Unmarshal(schemaJSON, &root); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	check := func(name string, typ reflect.Type, obj schemaObject) {
		all, req := jsonFields(typ)
		if got := keys(obj.Properties); !reflect.DeepEqual(got, all) {
			t.Errorf("%s: schema properties %v, Go fields %v", name, got, all)
		}
		if got := sorted(obj.Required); !reflect.DeepEqual(got, req) && !(len(got) == 0 && len(req) == 0) {
			t.Errorf("%s: schema required %v, Go required %v", name, got, req)
		}
	}
	check("EgressIntent", reflect.TypeOf(EgressIntent{}), root)
	for _, typ := range []reflect.Type{
		reflect.TypeOf(Owner{}), reflect.TypeOf(Listeners{}), reflect.TypeOf(Rule{}), reflect.TypeOf(Port{}),
		reflect.TypeOf(RuleTLS{}), reflect.TypeOf(Credential{}), reflect.TypeOf(TLS{}), reflect.TypeOf(CA{}), reflect.TypeOf(Certificate{}),
		reflect.TypeOf(Identity{}), reflect.TypeOf(SPIFFE{}),
	} {
		def, ok := root.Defs[typ.Name()]
		if !ok {
			t.Errorf("schema has no $defs/%s", typ.Name())
			continue
		}
		check(typ.Name(), typ, def)
	}
}

// The example decodes into the types with no unknown field and encodes back
// to the same document.
func TestExampleRoundTrips(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader(exampleJSON))
	dec.DisallowUnknownFields()
	var in EgressIntent
	if err := dec.Decode(&in); err != nil {
		t.Fatalf("example does not decode: %v", err)
	}
	if in.APIVersion != APIVersion || in.Kind != Kind {
		t.Errorf("apiVersion/kind = %s/%s", in.APIVersion, in.Kind)
	}
	out, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	_ = json.Unmarshal(exampleJSON, &a)
	_ = json.Unmarshal(out, &b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("round trip changed the document:\n%s", out)
	}
}
