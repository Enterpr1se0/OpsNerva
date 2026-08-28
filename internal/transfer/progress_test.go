package transfer

import (
	"bytes"
	"testing"
)

func TestWriterPublishesCommonProgressContract(t *testing.T) {
	var destination bytes.Buffer
	var updates []Progress
	w := NewWriter(&destination, 3, func(progress Progress) { updates = append(updates, progress) })
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	w.Finish()
	if destination.String() != "abc" {
		t.Fatalf("written content = %q", destination.String())
	}
	if len(updates) != 2 || updates[0] != (Progress{Total: 3}) || updates[1] != (Progress{Transferred: 3, Total: 3}) {
		t.Fatalf("progress updates = %#v", updates)
	}
}
