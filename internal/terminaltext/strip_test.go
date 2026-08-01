package terminaltext

import "testing"

func TestStripperCarriesSplitANSISequence(t *testing.T) {
	var stripper Stripper
	if got := stripper.WriteString("before\x1b[?20"); got != "before" {
		t.Fatalf("first chunk = %q", got)
	}
	if got := stripper.WriteString("04lafter\x1b]0;title\x1b"); got != "after" {
		t.Fatalf("second chunk = %q", got)
	}
	if got := stripper.WriteString("\\done"); got != "done" {
		t.Fatalf("third chunk = %q", got)
	}
}
