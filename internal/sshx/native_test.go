package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/config"
	"github.com/Enterpr1se0/opsnerva/internal/domain"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type testSSHServer struct {
	listener         net.Listener
	signer           ssh.Signer
	password         string
	root             string
	allowedPublicKey ssh.PublicKey
	wg               sync.WaitGroup
}

type shellInputBuffer struct {
	bytes.Buffer
}

func (*shellInputBuffer) Close() error { return nil }

type pausedUploadReader struct {
	reads   int
	waiting chan struct{}
	release chan struct{}
}

func (reader *pausedUploadReader) Read(data []byte) (int, error) {
	if reader.reads == 0 {
		reader.reads++
		return copy(data, bytes.Repeat([]byte("x"), 32*1024)), nil
	}
	if reader.reads == 1 {
		reader.reads++
		close(reader.waiting)
		<-reader.release
	}
	return 0, io.EOF
}

func TestNativeShellInterruptWritesPTYControlByte(t *testing.T) {
	input := &shellInputBuffer{}
	session := &nativeShellSession{stdin: input, done: make(chan struct{})}
	if err := session.Interrupt(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input.Bytes(), []byte{0x03}) {
		t.Fatalf("interrupt bytes = %v, want [3]", input.Bytes())
	}
}

func startTestSSHServer(t *testing.T, password string) *testSSHServer {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &testSSHServer{listener: listener, signer: signer, password: password, root: t.TempDir()}
	server.wg.Add(1)
	go server.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		server.wg.Wait()
	})
	return server
}

func (s *testSSHServer) host() domain.Host {
	host, portText, _ := net.SplitHostPort(s.listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	return domain.Host{
		ID: "host_native_test", Name: "native-test", Address: host, Port: port, User: "ops",
		AuthType: "password", Password: s.password,
	}
}

func testSFTPPath(localPath string) string {
	remotePath := filepath.ToSlash(localPath)
	if filepath.VolumeName(localPath) != "" {
		return "/" + remotePath
	}
	return remotePath
}

func TestSFTPConnectionKeyTracksCredentialsAndRoute(t *testing.T) {
	connection := ConnectionSpec{Target: domain.Host{
		ID: "target", Address: "192.0.2.10", Port: 22, User: "ops", AuthType: "password", Password: "first",
	}}
	original := sftpConnectionKey(connection)
	changedPassword := connection
	changedPassword.Target.Password = "second"
	if sftpConnectionKey(changedPassword) == original {
		t.Fatal("SFTP pool key ignored a credential change")
	}
	changedRoute := connection
	changedRoute.Jumps = []domain.Host{{ID: "jump", Address: "192.0.2.20", Port: 22, User: "relay", AuthType: "password", Password: "jump-secret"}}
	if sftpConnectionKey(changedRoute) == original {
		t.Fatal("SFTP pool key ignored a jump-host route change")
	}
}

func (s *testSSHServer) serve() {
	defer s.wg.Done()
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer connection.Close()
			serverConfig := &ssh.ServerConfig{
				PasswordCallback: func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
					if string(password) != s.password {
						return nil, fmt.Errorf("invalid password")
					}
					return nil, nil
				},
			}
			serverConfig.PublicKeyCallback = func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
				if s.allowedPublicKey == nil || !bytes.Equal(key.Marshal(), s.allowedPublicKey.Marshal()) {
					return nil, fmt.Errorf("invalid public key")
				}
				return nil, nil
			}
			serverConfig.AddHostKey(s.signer)
			_, channels, requests, err := ssh.NewServerConn(connection, serverConfig)
			if err != nil {
				return
			}
			go ssh.DiscardRequests(requests)
			for newChannel := range channels {
				if newChannel.ChannelType() == "direct-tcpip" {
					s.wg.Add(1)
					go func() {
						defer s.wg.Done()
						s.handleDirectTCPIP(newChannel)
					}()
					continue
				}
				if newChannel.ChannelType() != "session" {
					_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported channel")
					continue
				}
				channel, channelRequests, err := newChannel.Accept()
				if err != nil {
					continue
				}
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					s.handleSession(channel, channelRequests)
				}()
			}
		}()
	}
}

func (s *testSSHServer) handleDirectTCPIP(newChannel ssh.NewChannel) {
	var request struct {
		DestinationAddress string
		DestinationPort    uint32
		OriginAddress      string
		OriginPort         uint32
	}
	if err := ssh.Unmarshal(newChannel.ExtraData(), &request); err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, "invalid direct-tcpip request")
		return
	}
	upstream, err := net.Dial("tcp", net.JoinHostPort(request.DestinationAddress, strconv.Itoa(int(request.DestinationPort))))
	if err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, "target unavailable")
		return
	}
	defer upstream.Close()
	channel, requests, err := newChannel.Accept()
	if err != nil {
		return
	}
	defer channel.Close()
	go ssh.DiscardRequests(requests)
	copyDone := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, channel); copyDone <- struct{}{} }()
	go func() { _, _ = io.Copy(channel, upstream); copyDone <- struct{}{} }()
	<-copyDone
}

func (s *testSSHServer) handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for request := range requests {
		switch request.Type {
		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
				_ = request.Reply(false, nil)
				return
			}
			_ = request.Reply(true, nil)
			if strings.Contains(payload.Command, "hang-forever") {
				for range requests {
				}
				return
			}
			if strings.Contains(payload.Command, "bash -s") {
				_, _ = io.ReadAll(channel)
			}
			_, _ = io.WriteString(channel, "native-ok\n")
			exitStatus := uint32(0)
			if strings.Contains(payload.Command, "exit-seven") {
				exitStatus = 7
			}
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exitStatus}))
			return
		case "subsystem":
			var payload struct{ Name string }
			if err := ssh.Unmarshal(request.Payload, &payload); err != nil || payload.Name != "sftp" {
				_ = request.Reply(false, nil)
				continue
			}
			_ = request.Reply(true, nil)
			server, err := sftp.NewServer(channel)
			if err == nil {
				_ = server.Serve()
				_ = server.Close()
			}
			return
		default:
			_ = request.Reply(false, nil)
		}
	}
}

func TestNativeSSHTrustExecAndExitStatus(t *testing.T) {
	server := startTestSSHServer(t, "native-password")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: knownHosts}, config.Default().Limits)
	t.Cleanup(func() { _ = transport.Close() })
	connection := ConnectionSpec{Target: server.host()}

	_, err := transport.Exec(context.Background(), connection, domain.ExecRequest{Mode: domain.ExecProgram, Program: "printf", Args: []string{"ok"}, TimeoutSeconds: 5})
	if err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("unknown host key was not rejected: %v", err)
	}
	key, err := transport.ScanHostKey(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	if key.Fingerprint == "" || key.Algorithm != ssh.KeyAlgoED25519 {
		t.Fatalf("unexpected scanned key: %#v", key)
	}
	if key.Trusted {
		t.Fatal("unrecorded host key was reported as trusted")
	}
	if _, ok := transport.StoredHostKey(connection.Target); ok {
		t.Fatal("unrecorded host key was found in known_hosts")
	}
	if _, err := transport.TrustHostKey(context.Background(), connection, "SHA256:not-the-key"); err == nil {
		t.Fatal("mismatched host key fingerprint was trusted")
	}
	trustedKey, err := transport.TrustHostKey(context.Background(), connection, key.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !trustedKey.Trusted {
		t.Fatal("trusted host key was not marked trusted")
	}
	storedKey, ok := transport.StoredHostKey(connection.Target)
	if !ok || !storedKey.Trusted || storedKey.Fingerprint != key.Fingerprint || storedKey.Algorithm != key.Algorithm {
		t.Fatalf("stored host key was not recovered from known_hosts: %#v found=%t", storedKey, ok)
	}
	rescannedKey, err := transport.ScanHostKey(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	if !rescannedKey.Trusted {
		t.Fatal("recorded host key was not detected on rescan")
	}

	var streamed strings.Builder
	result, err := transport.ExecStream(context.Background(), connection, domain.ExecRequest{Mode: domain.ExecProgram, Program: "printf", Args: []string{"ok"}, TimeoutSeconds: 5}, func(stream string, data []byte) {
		if stream == "stdout" {
			streamed.Write(data)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "native-ok\n" || streamed.String() != "native-ok\n" {
		t.Fatalf("unexpected native SSH result: %#v streamed=%q", result, streamed.String())
	}

	result, err = transport.Exec(context.Background(), connection, domain.ExecRequest{Mode: domain.ExecProgram, Program: "exit-seven", TimeoutSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("remote exit status was not preserved: %#v", result)
	}
}

func TestNativeSSHRequiresKnownHostsPath(t *testing.T) {
	server := startTestSSHServer(t, "native-password")
	transport := NewNativeSSHTransport(config.SSH{}, config.Default().Limits)
	connection := ConnectionSpec{Target: server.host()}

	if _, err := transport.TrustHostKey(context.Background(), connection, "SHA256:test"); err == nil || !strings.Contains(err.Error(), "known_hosts path is not configured") {
		t.Fatalf("missing known_hosts path returned an unclear trust error: %v", err)
	}
	if _, err := transport.Exec(context.Background(), connection, domain.ExecRequest{Mode: domain.ExecProgram, Program: "true", TimeoutSeconds: 5}); err == nil || !strings.Contains(err.Error(), "known_hosts path is not configured") {
		t.Fatalf("missing known_hosts path returned an unclear execution error: %v", err)
	}
}

func TestNativeSFTPUpload(t *testing.T) {
	server := startTestSSHServer(t, "sftp-password")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: knownHosts}, config.Default().Limits)
	connection := ConnectionSpec{Target: server.host()}
	key, err := transport.ScanHostKey(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.TrustHostKey(context.Background(), connection, key.Fingerprint); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(t.TempDir(), "artifact.txt")
	if err := os.WriteFile(localPath, []byte("native sftp payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	remotePath := filepath.Join(server.root, "uploaded.txt")
	result, err := transport.Exec(context.Background(), connection, domain.ExecRequest{
		Mode: domain.ExecWorkspaceUpload, LocalPath: localPath, RemotePath: testSFTPPath(remotePath), TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("unexpected SFTP result: %#v", result)
	}
	data, err := os.ReadFile(remotePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "native sftp payload" {
		t.Fatalf("unexpected uploaded data %q", data)
	}
}

func TestNativeSFTPFileManagerOperations(t *testing.T) {
	server := startTestSSHServer(t, "sftp-manager-password")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: knownHosts}, config.Default().Limits)
	t.Cleanup(func() { _ = transport.Close() })
	connection := ConnectionSpec{Target: server.host()}
	connection.Target.ID = "sftp_manager_host"
	key, err := transport.ScanHostKey(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.TrustHostKey(context.Background(), connection, key.Fingerprint); err != nil {
		t.Fatal(err)
	}

	directoryLocal := filepath.Join(server.root, "managed")
	directory := testSFTPPath(directoryLocal)
	created, err := transport.CreateSFTPDirectory(context.Background(), connection, directory)
	if err != nil {
		t.Fatal(err)
	}
	if created.Type != "directory" || created.Path != directory {
		t.Fatalf("unexpected created directory: %#v", created)
	}
	poolKey := sftpConnectionKey(connection)
	transport.sftpPoolMu.Lock()
	pooledEntry := transport.sftpPool[poolKey]
	pooledReady := pooledEntry != nil && pooledEntry.client != nil && pooledEntry.refs == 0
	transport.sftpPoolMu.Unlock()
	if !pooledReady {
		t.Fatalf("SFTP base connection was not retained after the first operation: %#v", pooledEntry)
	}

	filePath := directory + "/hello.txt"
	fileLocal := filepath.Join(directoryLocal, "hello.txt")
	uploaded, err := transport.UploadSFTPFile(context.Background(), connection, filePath, strings.NewReader("hello over SFTP"), false)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.Type != "file" || uploaded.Size != int64(len("hello over SFTP")) {
		t.Fatalf("unexpected uploaded entry: %#v", uploaded)
	}
	transport.sftpPoolMu.Lock()
	reusedEntry := transport.sftpPool[poolKey]
	transport.sftpPoolMu.Unlock()
	if reusedEntry != pooledEntry {
		t.Fatal("sequential SFTP operations did not reuse the base SSH connection")
	}
	if _, err := transport.UploadSFTPFile(context.Background(), connection, filePath, strings.NewReader("conflict"), false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("upload conflict was not rejected: %v", err)
	}

	listed, err := transport.ListSFTPFiles(context.Background(), connection, directory)
	if err != nil {
		t.Fatal(err)
	}
	if listed.HostID != connection.Target.ID || len(listed.Entries) != 1 || listed.Entries[0].Name != "hello.txt" {
		t.Fatalf("unexpected directory listing: %#v", listed)
	}

	download, err := transport.OpenSFTPFile(context.Background(), connection, filePath)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := io.ReadAll(download.Reader)
	if closeErr := download.Reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(downloaded) != "hello over SFTP" {
		t.Fatalf("unexpected downloaded content %q", downloaded)
	}
	if err := os.Chmod(fileLocal, 0o750); err != nil {
		t.Fatal(err)
	}
	replaced, err := transport.UploadSFTPFile(context.Background(), connection, filePath, strings.NewReader("edited over SFTP"), true)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Size != int64(len("edited over SFTP")) {
		t.Fatalf("unexpected replaced entry: %#v", replaced)
	}
	replacedInfo, err := os.Stat(fileLocal)
	if err != nil || (runtime.GOOS != "windows" && replacedInfo.Mode().Perm() != 0o750) {
		t.Fatalf("SFTP overwrite mode = %v, err = %v", replacedInfo.Mode().Perm(), err)
	}
	replacedContent, err := os.ReadFile(fileLocal)
	if err != nil || string(replacedContent) != "edited over SFTP" {
		t.Fatalf("SFTP overwrite content = %q, err = %v", replacedContent, err)
	}

	renamedPath := directory + "/renamed.txt"
	renamed, err := transport.RenameSFTPEntry(context.Background(), connection, filePath, renamedPath)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Path != renamedPath || renamed.Name != "renamed.txt" {
		t.Fatalf("unexpected renamed entry: %#v", renamed)
	}
	if _, err := transport.RemoveSFTPEntry(context.Background(), connection, renamedPath, false); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RemoveSFTPEntry(context.Background(), connection, directory, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directoryLocal); !os.IsNotExist(err) {
		t.Fatalf("managed directory still exists: %v", err)
	}
}

func TestNativeSFTPCancelledUploadRemovesTemporaryFile(t *testing.T) {
	server := startTestSSHServer(t, "sftp-cancel-password")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: knownHosts}, config.Default().Limits)
	t.Cleanup(func() { _ = transport.Close() })
	connection := ConnectionSpec{Target: server.host()}
	connection.Target.ID = "sftp_cancel_host"
	key, err := transport.ScanHostKey(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.TrustHostKey(context.Background(), connection, key.Fingerprint); err != nil {
		t.Fatal(err)
	}
	directoryLocal := filepath.Join(server.root, "cancelled-upload")
	if err := os.Mkdir(directoryLocal, 0o755); err != nil {
		t.Fatal(err)
	}
	remotePath := testSFTPPath(filepath.Join(directoryLocal, "artifact.bin"))
	reader := &pausedUploadReader{waiting: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := transport.UploadSFTPFile(ctx, connection, remotePath, reader, false)
		result <- err
	}()
	select {
	case <-reader.waiting:
	case <-time.After(2 * time.Second):
		close(reader.release)
		t.Fatal("upload did not reach the paused source read")
	}
	cancel()
	poolKey := sftpConnectionKey(connection)
	closed := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		transport.sftpPoolMu.Lock()
		entry := transport.sftpPool[poolKey]
		closed = entry != nil && entry.refs == 0
		transport.sftpPoolMu.Unlock()
		if closed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(reader.release)
	if !closed {
		t.Fatal("cancellation did not close the active SFTP lease")
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled upload unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled upload did not return")
	}
	entries, err := os.ReadDir(directoryLocal)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("cancelled upload left remote files: %v", names)
	}
}

func TestNativeSFTPTransfersFileBetweenHostsAtomically(t *testing.T) {
	sourceServer := startTestSSHServer(t, "source-password")
	destinationServer := startTestSSHServer(t, "destination-password")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: knownHosts}, config.Default().Limits)
	source := ConnectionSpec{Target: sourceServer.host()}
	source.Target.ID = "source_host"
	source.Target.Name = "source"
	destination := ConnectionSpec{Target: destinationServer.host()}
	destination.Target.ID = "destination_host"
	destination.Target.Name = "destination"
	for _, connection := range []ConnectionSpec{source, destination} {
		key, err := transport.ScanHostKey(context.Background(), connection)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transport.TrustHostKey(context.Background(), connection, key.Fingerprint); err != nil {
			t.Fatal(err)
		}
	}

	content := []byte("host-to-host transfer payload\n")
	sourcePath := filepath.Join(sourceServer.root, "release.bin")
	if err := os.WriteFile(sourcePath, content, 0o640); err != nil {
		t.Fatal(err)
	}
	destinationPath := filepath.Join(destinationServer.root, "release.bin")
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	var progress [][2]int64
	result, err := transport.TransferFile(context.Background(), source, destination, domain.ExecRequest{
		Mode: domain.ExecSSHFileTransfer, SourceHostID: source.Target.ID, SourcePath: testSFTPPath(sourcePath),
		HostID: destination.Target.ID, RemotePath: testSFTPPath(destinationPath), ExpectedSHA256: digest, TimeoutSeconds: 5,
	}, func(transferred, total int64) { progress = append(progress, [2]int64{transferred, total}) })
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !strings.Contains(string(result.Stdout), digest) {
		t.Fatalf("unexpected transfer result: %#v", result)
	}
	if len(progress) < 2 || progress[0] != [2]int64{0, int64(len(content))} || progress[len(progress)-1] != [2]int64{int64(len(content)), int64(len(content))} {
		t.Fatalf("unexpected transfer progress: %#v", progress)
	}
	transferred, err := os.ReadFile(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(transferred, content) {
		t.Fatalf("destination content mismatch: %q", transferred)
	}
	info, err := os.Stat(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("destination mode=%o, want 640", info.Mode().Perm())
	}
}

func TestNativeSFTPTransferConflictLeavesDestinationUntouched(t *testing.T) {
	sourceServer := startTestSSHServer(t, "source-conflict-password")
	destinationServer := startTestSSHServer(t, "destination-conflict-password")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: knownHosts}, config.Default().Limits)
	source := ConnectionSpec{Target: sourceServer.host()}
	source.Target.ID = "source_conflict_host"
	destination := ConnectionSpec{Target: destinationServer.host()}
	destination.Target.ID = "destination_conflict_host"
	for _, connection := range []ConnectionSpec{source, destination} {
		key, err := transport.ScanHostKey(context.Background(), connection)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transport.TrustHostKey(context.Background(), connection, key.Fingerprint); err != nil {
			t.Fatal(err)
		}
	}

	sourcePath := filepath.Join(sourceServer.root, "source.bin")
	destinationPath := filepath.Join(destinationServer.root, "destination.bin")
	if err := os.WriteFile(sourcePath, []byte("changed source"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := []byte("keep destination")
	if err := os.WriteFile(destinationPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	destinationDigest := fmt.Sprintf("%x", sha256.Sum256(original))
	_, err := transport.TransferFile(context.Background(), source, destination, domain.ExecRequest{
		Mode: domain.ExecSSHFileTransfer, SourceHostID: source.Target.ID, SourcePath: testSFTPPath(sourcePath),
		HostID: destination.Target.ID, RemotePath: testSFTPPath(destinationPath), ExpectedSHA256: strings.Repeat("0", 64),
		ExpectedDestinationSHA256: destinationDigest, TimeoutSeconds: 5,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "source file version conflict") {
		t.Fatalf("source version conflict was not reported: %v", err)
	}
	current, readErr := os.ReadFile(destinationPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(current, original) {
		t.Fatalf("destination changed after source conflict: %q", current)
	}
	matches, err := filepath.Glob(filepath.Join(destinationServer.root, ".opsnerva-transfer-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files were not cleaned up: matches=%v err=%v", matches, err)
	}
}

func TestNativeSSHProxyJump(t *testing.T) {
	jumpServer := startTestSSHServer(t, "jump-password")
	targetServer := startTestSSHServer(t, "target-password")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: knownHosts}, config.Default().Limits)

	jump := jumpServer.host()
	jump.Name = "jump"
	jumpConnection := ConnectionSpec{Target: jump}
	jumpKey, err := transport.ScanHostKey(context.Background(), jumpConnection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.TrustHostKey(context.Background(), jumpConnection, jumpKey.Fingerprint); err != nil {
		t.Fatal(err)
	}

	target := targetServer.host()
	target.Name = "target"
	connection := ConnectionSpec{Target: target, Jumps: []domain.Host{jump}}
	targetKey, err := transport.ScanHostKey(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.TrustHostKey(context.Background(), connection, targetKey.Fingerprint); err != nil {
		t.Fatal(err)
	}
	result, err := transport.Exec(context.Background(), connection, domain.ExecRequest{Mode: domain.ExecProgram, Program: "printf", Args: []string{"via-jump"}, TimeoutSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "native-ok\n" {
		t.Fatalf("unexpected ProxyJump result: %#v", result)
	}
}

func TestNativeSSHContextCancellationClosesSession(t *testing.T) {
	server := startTestSSHServer(t, "cancel-password")
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: knownHosts}, config.Default().Limits)
	connection := ConnectionSpec{Target: server.host()}
	key, err := transport.ScanHostKey(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.TrustHostKey(context.Background(), connection, key.Fingerprint); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, execErr := transport.Exec(ctx, connection, domain.ExecRequest{Mode: domain.ExecProgram, Program: "hang-forever", TimeoutSeconds: 5})
		done <- execErr
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected cancellation error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("native SSH execution did not stop after context cancellation")
	}
}

func TestNativeSSHPrivateKeyAuthentication(t *testing.T) {
	server := startTestSSHServer(t, "unused-password")
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSigner, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	server.allowedPublicKey = clientSigner.PublicKey()
	block, err := ssh.MarshalPrivateKey(privateKey, "native-test")
	if err != nil {
		t.Fatal(err)
	}
	privateKeyData := pem.EncodeToMemory(block)
	host := server.host()
	host.AuthType = "key"
	host.Password = ""
	host.PrivateKey = privateKeyData
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	transport := NewNativeSSHTransport(config.SSH{DefaultKnownHosts: knownHosts}, config.Default().Limits)
	connection := ConnectionSpec{Target: host}
	key, err := transport.ScanHostKey(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.TrustHostKey(context.Background(), connection, key.Fingerprint); err != nil {
		t.Fatal(err)
	}
	result, err := transport.Exec(context.Background(), connection, domain.ExecRequest{Mode: domain.ExecProgram, Program: "printf", TimeoutSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("unexpected key-auth result: %#v", result)
	}
}
