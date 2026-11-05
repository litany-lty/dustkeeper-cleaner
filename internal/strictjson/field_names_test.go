package strictjson

import "testing"

func TestExactFieldsPreservesMapKeysAndEmbeddedFields(t *testing.T) {
	type Embedded struct {
		Limit int `json:"limit"`
	}
	var target struct {
		Embedded
		ByName map[string]Embedded `json:"by_name"`
	}
	if err := Unmarshal([]byte(`{"limit":1,"by_name":{"A":{"limit":2},"a":{"limit":3}}}`), &target, Defaults()); err != nil {
		t.Fatal(err)
	}
	if len(target.ByName) != 2 || target.Limit != 1 {
		t.Fatalf("bad decode: %+v", target)
	}
}
