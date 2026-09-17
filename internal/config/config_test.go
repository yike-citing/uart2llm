package config

import (
	"encoding/json"
	"testing"
)

func TestTransactionalValidationAndPersistence(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	for _, patch := range []string{`{"api_listen":"0.0.0.0:8765"}`, `{"upstream_url":"http://insecure"}`, `{"max_concurrent":5}`, `{"baud":null}`, `{"typo":1}`} {
		var p map[string]json.RawMessage
		json.Unmarshal([]byte(patch), &p)
		if _, e = s.Update(p); e == nil {
			t.Fatalf("accepted %s", patch)
		}
		if s.Get() != Default() {
			t.Fatal("invalid patch altered settings")
		}
	}
	if _, e = s.Update(map[string]json.RawMessage{"baud": json.RawMessage("921600")}); e != nil {
		t.Fatal(e)
	}
	s, e = Open(dir)
	if e != nil || s.Get().Baud != 921600 {
		t.Fatal("config not persisted")
	}
}
