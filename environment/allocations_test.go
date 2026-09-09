package environment

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The Panel is PHP, and json_encode renders an empty array and an empty map
// identically as []. A server with no allocations therefore arrives with
// "mappings": [], which will not decode into a map and made the server
// impossible to create rather than merely unallocated.
func TestAllocationsAcceptsAnEmptyArrayForMappings(t *testing.T) {
	var a Allocations
	if err := json.Unmarshal([]byte(`{"default":{"ip":"0.0.0.0","port":0},"mappings":[]}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(a.Mappings) != 0 {
		t.Errorf("mappings = %v, want empty", a.Mappings)
	}
	if got := a.Bindings(); len(got) != 0 {
		t.Errorf("Bindings = %v, want empty", got)
	}
}

func TestAllocationsDecodesAnObjectOfMappings(t *testing.T) {
	var a Allocations
	const body = `{"default":{"ip":"1.2.3.4","port":25565},` +
		`"mappings":{"1.2.3.4":[25565,25566],"0.0.0.0":[27015]}}`
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := PortMappings{"1.2.3.4": {25565, 25566}, "0.0.0.0": {27015}}
	if !reflect.DeepEqual(a.Mappings, want) {
		t.Errorf("mappings = %v, want %v", a.Mappings, want)
	}
	if a.DefaultMapping.Port != 25565 {
		t.Errorf("default port = %d, want 25565", a.DefaultMapping.Port)
	}
}

func TestAllocationsRejectsANonEmptyArray(t *testing.T) {
	// Silently reading this as "no allocations" would produce a server that
	// starts and is unreachable, which is harder to diagnose than a decode error.
	var a Allocations
	err := json.Unmarshal([]byte(`{"mappings":[{"ip":"1.2.3.4"}]}`), &a)
	if err == nil {
		t.Fatal("expected an error for a non-empty array")
	}
	if !strings.Contains(err.Error(), "non-empty JSON array") {
		t.Errorf("error = %v, want it to name the shape problem", err)
	}
}

func TestAllocationsAcceptsNullMappings(t *testing.T) {
	var a Allocations
	if err := json.Unmarshal([]byte(`{"mappings":null}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Mappings != nil {
		t.Errorf("mappings = %v, want nil", a.Mappings)
	}
}

// Round-tripping must still produce an object, so a daemon feeding another
// component does not propagate the shape it just had to tolerate.
func TestAllocationsMarshalsMappingsAsAnObject(t *testing.T) {
	b, err := json.Marshal(Allocations{Mappings: PortMappings{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"mappings":{}`) {
		t.Errorf("marshalled as %s, want an object for mappings", b)
	}
}
