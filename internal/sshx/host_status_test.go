package sshx

import "testing"

func TestParseHostStatus(t *testing.T) {
	status, err := parseHostStatus("cpu\t1000\t250\n" +
		"memory\t3221225472\t8589934592\n" +
		"disk\t10737418240\t53687091200\n" +
		"network\t123456\t654321\n" +
		"uptime\t93784\n")
	if err != nil {
		t.Fatal(err)
	}
	if status.CPUTotal != 1000 || status.CPUIdle != 250 || status.MemoryUsedBytes != 3221225472 || status.MemoryTotalBytes != 8589934592 || status.DiskUsedBytes != 10737418240 || status.DiskTotalBytes != 53687091200 || status.NetworkReceivedBytes != 123456 || status.NetworkSentBytes != 654321 || status.UptimeSeconds != 93784 {
		t.Fatalf("unexpected host status: %#v", status)
	}
}

func TestParseHostStatusRejectsIncompleteOutput(t *testing.T) {
	if _, err := parseHostStatus("cpu 1000 250\n"); err == nil {
		t.Fatal("incomplete host status was accepted")
	}
}
