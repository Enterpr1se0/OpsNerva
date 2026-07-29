package service

import "testing"

func TestPartialExecutionStatusIsTerminal(t *testing.T) {
	if !terminalExecutionStatus("partial") {
		t.Fatal("partial execution did not release its live execution owner")
	}
	if terminalExecutionStatus("running") {
		t.Fatal("running execution was classified as terminal")
	}
}
