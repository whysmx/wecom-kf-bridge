package runtime

import "testing"

func TestExampleConfigParses(t *testing.T) {
	if _, err := LoadConfig("../config.example.json"); err != nil {
		t.Fatal(err)
	}
}
