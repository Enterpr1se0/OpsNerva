package sshx

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Hold replies until multiple requests have arrived. A serial client cannot
// pass this gate; a pipelined client can. No wall-clock throughput assertion.
type sftpPacketGate struct {
	io.ReadWriteCloser
	packetType byte
	threshold  int32
	count      atomic.Int32
	entered    chan struct{}
	release    chan struct{}
	once       sync.Once
	pending    []byte
}

func (gate *sftpPacketGate) unblock() { gate.once.Do(func() { close(gate.release) }) }

func (gate *sftpPacketGate) Read(data []byte) (int, error) {
	if len(gate.pending) == 0 {
		var header [4]byte
		if _, err := io.ReadFull(gate.ReadWriteCloser, header[:]); err != nil {
			return 0, err
		}
		body := make([]byte, binary.BigEndian.Uint32(header[:]))
		if _, err := io.ReadFull(gate.ReadWriteCloser, body); err != nil {
			return 0, err
		}
		if len(body) > 0 && body[0] == gate.packetType {
			count := gate.count.Add(1)
			if count == 1 {
				close(gate.entered)
			}
			if gate.threshold > 0 && count >= gate.threshold {
				gate.unblock()
			}
		}
		gate.pending = append(header[:], body...)
	}
	n := copy(data, gate.pending)
	gate.pending = gate.pending[n:]
	return n, nil
}

func (gate *sftpPacketGate) Write(data []byte) (int, error) {
	if gate.count.Load() > 0 {
		<-gate.release
	}
	return gate.ReadWriteCloser.Write(data)
}

func gatedSFTPServer(t *testing.T, packetType byte, threshold int32) (*testSSHServer, <-chan *sftpPacketGate) {
	t.Helper()
	gates := make(chan *sftpPacketGate, 4)
	var first atomic.Bool
	server := startTestSSHServer(t, "sftp-gated-password", func(server *testSSHServer) {
		server.sftpHandler = func(channel ssh.Channel, request *ssh.Request) {
			_ = request.Reply(true, nil)
			var stream io.ReadWriteCloser = channel
			if first.CompareAndSwap(false, true) {
				gate := &sftpPacketGate{ReadWriteCloser: channel, packetType: packetType, threshold: threshold, entered: make(chan struct{}), release: make(chan struct{})}
				gates <- gate
				stream = gate
			}
			sftpServer, err := sftp.NewServer(stream)
			if err == nil {
				_ = sftpServer.Serve()
				_ = sftpServer.Close()
			}
		}
	})
	return server, gates
}

func TestSFTPTransfersPipelinePackets(t *testing.T) {
	for _, operation := range []string{"download", "upload"} {
		t.Run(operation, func(t *testing.T) {
			packet := byte(5) // SSH_FXP_READ
			if operation == "upload" {
				packet = 6 // SSH_FXP_WRITE
			}
			server, gates := gatedSFTPServer(t, packet, 2)
			transport, connection := testSFTPConnection(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lease, err := transport.openSFTP(ctx, connection)
			if err != nil {
				t.Fatal(err)
			}
			gate := <-gates
			defer gate.unblock()
			_ = lease.Close()
			content := bytes.Repeat([]byte("pipelined SFTP bytes\x00\xff"), 32768)
			local := filepath.Join(server.root, "pipeline.bin")
			remote := testSFTPPath(local)
			if operation == "download" {
				if err := os.WriteFile(local, content, 0o600); err != nil {
					t.Fatal(err)
				}
				download, err := transport.OpenSFTPFile(ctx, connection, remote)
				if err != nil {
					t.Fatal(err)
				}
				var received bytes.Buffer
				_, err = io.Copy(&received, download.Reader)
				closeErr := download.Reader.Close()
				if err != nil || closeErr != nil || !bytes.Equal(received.Bytes(), content) {
					t.Fatalf("pipelined download failed: copy=%v, close=%v, bytes=%d", err, closeErr, received.Len())
				}
			} else {
				// Deliberately hide Len/Size/Stat/WriterTo, as with an HTTP body.
				source := struct{ io.Reader }{bytes.NewReader(content)}
				if _, err := transport.UploadSFTPFile(ctx, connection, remote, source, false); err != nil {
					t.Fatal(err)
				}
				stored, err := os.ReadFile(local)
				if err != nil || !bytes.Equal(stored, content) {
					t.Fatalf("pipelined upload content mismatch: %v", err)
				}
			}
			if gate.count.Load() < 2 {
				t.Fatal("transfer did not pipeline packets")
			}
		})
	}
}

func TestSFTPDownloadCancellationUnblocksFileLocks(t *testing.T) {
	for _, packet := range []byte{7, 5} { // SSH_FXP_LSTAT, SSH_FXP_READ
		t.Run(map[byte]string{7: "metadata", 5: "reading"}[packet], func(t *testing.T) {
			server, gates := gatedSFTPServer(t, packet, 0)
			transport, connection := testSFTPConnection(t, server)
			local := filepath.Join(server.root, "blocked.bin")
			if err := os.WriteFile(local, bytes.Repeat([]byte("x"), 128<<10), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				download, err := transport.OpenSFTPFile(ctx, connection, testSFTPPath(local))
				if err == nil {
					_, err = io.Copy(io.Discard, download.Reader)
					err = errors.Join(err, download.Reader.Close())
				}
				done <- err
			}()
			gate := <-gates
			defer gate.unblock()
			awaitSFTP(t, gate.entered, "blocked remote operation")
			// The second pool slot must remain usable during and after cancellation.
			other, err := transport.openSFTP(context.Background(), connection)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled blocked download succeeded")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation remained blocked by a file lock")
			}
			if _, err := other.client.Getwd(); err != nil {
				t.Fatalf("cancellation interrupted another connection: %v", err)
			}
		})
	}
}

func TestSFTPPipelinedUploadFailurePreservesDestination(t *testing.T) {
	server := startTestSSHServer(t, "sftp-failed-source-password")
	transport, connection := testSFTPConnection(t, server)
	local := filepath.Join(server.root, "existing.bin")
	original := []byte("must not be replaced")
	if err := os.WriteFile(local, original, 0o600); err != nil {
		t.Fatal(err)
	}
	// Keep the other slot busy: failure cleanup must release the upload lease
	// before borrowing a session, rather than waiting on its own full pool.
	other, err := transport.openSFTP(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	sourceErr := errors.New("upload source failed")
	source := io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("x"), 256<<10)), iotest.ErrReader(sourceErr))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := transport.UploadSFTPFile(ctx, connection, testSFTPPath(local), source, true); !errors.Is(err, sourceErr) {
		t.Fatalf("unexpected upload error: %v", err)
	}
	stored, err := os.ReadFile(local)
	if err != nil || !bytes.Equal(stored, original) {
		t.Fatalf("failed upload changed destination: %v", err)
	}
	entries, err := os.ReadDir(server.root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "existing.bin" {
		t.Fatalf("failed upload left temporary files: %v, %v", entries, err)
	}
	if _, err := other.client.Getwd(); err != nil {
		t.Fatalf("cleanup interrupted the other session: %v", err)
	}
}

func TestSFTPBlockedUploadCancellationCleansTemporaryFile(t *testing.T) {
	server, gates := gatedSFTPServer(t, 6, 0) // SSH_FXP_WRITE
	transport, connection := testSFTPConnection(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := transport.UploadSFTPFile(ctx, connection, testSFTPPath(filepath.Join(server.root, "canceled.bin")), bytes.NewReader(bytes.Repeat([]byte("x"), 128<<10)), false)
		done <- err
	}()
	gate := <-gates
	defer gate.unblock()
	awaitSFTP(t, gate.entered, "blocked remote upload")
	other, err := transport.openSFTP(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled upload succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled upload or cleanup remained blocked")
	}
	entries, err := os.ReadDir(server.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled upload left remote files: %v, %v", entries, err)
	}
	if _, err := other.client.Getwd(); err != nil {
		t.Fatalf("upload cancellation interrupted the other session: %v", err)
	}
}
