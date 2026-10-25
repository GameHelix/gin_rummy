package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadJSONRejectsTrailingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.json")
	for _, bad := range []string{"{}]", "{}}", "{}]true", "{} {}"} {
		os.WriteFile(path, []byte(bad), 0o600)
		var v map[string]any
		if err := ReadJSON(path, &v); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	os.WriteFile(path, []byte("{\"a\": 1}\n"), 0o600)
	var v map[string]any
	if err := ReadJSON(path, &v); err != nil {
		t.Fatal(err)
	}
}
