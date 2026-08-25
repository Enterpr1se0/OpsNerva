package service

import "testing"

func TestTerminalExecutionStatuses(t *testing.T) {
	if !terminalExecutionStatus("partial") {
		t.Fatal("partial execution did not release its live execution owner")
	}
	if !terminalExecutionStatus("cancelled") {
		t.Fatal("cancelled task would keep waiting for another terminal state")
	}
	if terminalExecutionStatus("running") {
		t.Fatal("running execution was classified as terminal")
	}
}
