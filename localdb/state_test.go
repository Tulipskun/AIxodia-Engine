package localdb

import (
	"path/filepath"
	"testing"
)

func TestPutStateThenGetState(t *testing.T) {
	handle, err := Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	if err := PutState(handle, "engine/settings", `{"sub_enabled":true}`); err != nil {
		t.Fatal(err)
	}
	value, found, err := GetState(handle, "engine/settings")
	if err != nil {
		t.Fatal(err)
	}
	if !found || value != `{"sub_enabled":true}` {
		t.Fatalf("GetState = %q, %v; want the value that was written", value, found)
	}
}

// A key that was never written is not an error: the phone asks for settings on
// a fresh install and has to be told the defaults, not a failure.
func TestGetStateOfAnUnwrittenKey(t *testing.T) {
	handle, err := Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	value, found, err := GetState(handle, "engine/settings")
	if err != nil {
		t.Fatal(err)
	}
	if found || value != "" {
		t.Fatalf("GetState = %q, %v; want no value and no error", value, found)
	}
}

func TestPutStateReplacesTheEarlierValue(t *testing.T) {
	handle, err := Open(filepath.Join(t.TempDir(), "aixodia.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	if err := PutState(handle, "engine/settings", "first"); err != nil {
		t.Fatal(err)
	}
	if err := PutState(handle, "engine/settings", "second"); err != nil {
		t.Fatal(err)
	}
	value, found, err := GetState(handle, "engine/settings")
	if err != nil {
		t.Fatal(err)
	}
	if !found || value != "second" {
		t.Fatalf("GetState = %q, %v; want second", value, found)
	}
}
