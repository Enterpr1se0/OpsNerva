package sshx

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/transfer"

	"github.com/pkg/sftp"
)

var transferDigestPattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

type transferSummary struct {
	SourceHostID      string `json:"source_host_id"`
	SourcePath        string `json:"source_path"`
	DestinationHostID string `json:"destination_host_id"`
	DestinationPath   string `json:"destination_path"`
	Bytes             int64  `json:"bytes"`
	SHA256            string `json:"sha256"`
	Mode              string `json:"mode"`
	Overwritten       bool   `json:"overwritten"`
}

func (t *NativeSSHTransport) TransferFile(ctx context.Context, source, destination ConnectionSpec, req domain.ExecRequest, progress transfer.Reporter) (RawResult, error) {
	if err := validateNativeConnection(source); err != nil {
		return RawResult{}, fmt.Errorf("invalid source SSH connection: %w", err)
	}
	if err := validateNativeConnection(destination); err != nil {
		return RawResult{}, fmt.Errorf("invalid destination SSH connection: %w", err)
	}
	if err := validateHostTransferRequest(req); err != nil {
		return RawResult{}, err
	}

	timeout := effectiveTimeout(req.TimeoutSeconds, t.limits)
	transferCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	started := time.Now()

	sourceClient, err := t.connect(transferCtx, source, nil, false)
	if err != nil {
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("connect source SSH for SFTP: %w", err)
	}
	defer sourceClient.Close()
	destinationClient, err := t.connect(transferCtx, destination, nil, false)
	if err != nil {
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("connect destination SSH for SFTP: %w", err)
	}
	defer destinationClient.Close()

	sourceSFTP, err := sftp.NewClient(sourceClient.client)
	if err != nil {
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("start source SFTP: %w", err)
	}
	defer sourceSFTP.Close()
	destinationSFTP, err := sftp.NewClient(destinationClient.client)
	if err != nil {
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("start destination SFTP: %w", err)
	}
	defer destinationSFTP.Close()
	stopCancellation := context.AfterFunc(transferCtx, func() {
		_ = sourceSFTP.Close()
		_ = destinationSFTP.Close()
		_ = sourceClient.Close()
		_ = destinationClient.Close()
	})
	defer stopCancellation()

	sourceInfo, err := sourceSFTP.Lstat(req.SourcePath)
	if err != nil {
		return transferFailure(ctx, transferCtx, timeout, started, "inspect source file", err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("source path is not a regular file")
	}
	if err := verifyTransferDestination(destinationSFTP, req); err != nil {
		if transferCtx.Err() != nil {
			return transferFailure(ctx, transferCtx, timeout, started, "verify destination file", err)
		}
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, err
	}

	sourceFile, err := sourceSFTP.Open(req.SourcePath)
	if err != nil {
		return transferFailure(ctx, transferCtx, timeout, started, "open source file", err)
	}
	tempPath, destinationFile, err := createTransferTemp(destinationSFTP, req.RemotePath)
	if err != nil {
		_ = sourceFile.Close()
		return transferFailure(ctx, transferCtx, timeout, started, "create destination temporary file", err)
	}
	tempExists := true
	defer func() {
		if tempExists {
			_ = destinationSFTP.Remove(tempPath)
		}
	}()

	digest := sha256.New()
	progressWriter := transfer.NewWriter(io.MultiWriter(destinationFile, digest), sourceInfo.Size(), progress)
	written, copyErr := io.Copy(progressWriter, sourceFile)
	progressWriter.Finish()
	sourceCloseErr := sourceFile.Close()
	destinationCloseErr := destinationFile.Close()
	if copyErr == nil {
		copyErr = sourceCloseErr
	}
	if copyErr == nil {
		copyErr = destinationCloseErr
	}
	if copyErr != nil {
		return transferFailure(ctx, transferCtx, timeout, started, "stream file between SSH hosts", copyErr)
	}
	actualDigest := hex.EncodeToString(digest.Sum(nil))
	if actualDigest != strings.ToLower(req.ExpectedSHA256) {
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("source file version conflict: expected SHA256 %s, got %s", strings.ToLower(req.ExpectedSHA256), actualDigest)
	}
	if err := destinationSFTP.Chmod(tempPath, sourceInfo.Mode().Perm()); err != nil {
		return transferFailure(ctx, transferCtx, timeout, started, "preserve destination file mode", err)
	}
	if err := verifyTransferDestination(destinationSFTP, req); err != nil {
		if transferCtx.Err() != nil {
			return transferFailure(ctx, transferCtx, timeout, started, "revalidate destination file", err)
		}
		return RawResult{ExitCode: -1, Duration: time.Since(started)}, err
	}
	replaceDestination := req.ExpectedDestinationSHA256 != ""
	if replaceDestination {
		if err := destinationSFTP.PosixRename(tempPath, req.RemotePath); err != nil {
			return transferFailure(ctx, transferCtx, timeout, started, "atomically replace destination file", err)
		}
	} else if err := destinationSFTP.Rename(tempPath, req.RemotePath); err != nil {
		return transferFailure(ctx, transferCtx, timeout, started, "atomically create destination file", err)
	}
	tempExists = false

	encoded, _ := json.Marshal(transferSummary{
		SourceHostID: req.SourceHostID, SourcePath: req.SourcePath,
		DestinationHostID: req.HostID, DestinationPath: req.RemotePath,
		Bytes: written, SHA256: actualDigest, Mode: sourceInfo.Mode().Perm().String(), Overwritten: replaceDestination,
	})
	return RawResult{ExitCode: 0, Stdout: append(encoded, '\n'), Duration: time.Since(started)}, nil
}

func validateHostTransferRequest(req domain.ExecRequest) error {
	if req.Mode != domain.ExecSSHFileTransfer {
		return fmt.Errorf("invalid host file transfer mode %q", req.Mode)
	}
	if req.SourceHostID == "" || req.HostID == "" || req.SourceHostID == req.HostID {
		return fmt.Errorf("source and destination SSH hosts must be different")
	}
	for name, value := range map[string]string{"source path": req.SourcePath, "destination path": req.RemotePath} {
		if !path.IsAbs(value) || path.Clean(value) != value || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("%s must be a clean absolute path", name)
		}
	}
	if !transferDigestPattern.MatchString(req.ExpectedSHA256) {
		return fmt.Errorf("source expected SHA256 is invalid")
	}
	if req.ExpectedDestinationSHA256 != "" && !transferDigestPattern.MatchString(req.ExpectedDestinationSHA256) {
		return fmt.Errorf("destination expected SHA256 is invalid")
	}
	return nil
}

func verifyTransferDestination(client *sftp.Client, req domain.ExecRequest) error {
	info, err := client.Lstat(req.RemotePath)
	if err != nil {
		if os.IsNotExist(err) {
			if req.ExpectedDestinationSHA256 != "" {
				return fmt.Errorf("destination file version conflict: expected an existing file")
			}
			return nil
		}
		return fmt.Errorf("inspect destination file: %w", err)
	}
	if req.ExpectedDestinationSHA256 == "" {
		return fmt.Errorf("destination file already exists; inspect it and provide its expected_destination_sha256 to replace that exact version")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("destination path is not a regular file")
	}
	actual, err := hashRemoteFile(client, req.RemotePath)
	if err != nil {
		return fmt.Errorf("hash destination file: %w", err)
	}
	if actual != strings.ToLower(req.ExpectedDestinationSHA256) {
		return fmt.Errorf("destination file version conflict: expected SHA256 %s, got %s", strings.ToLower(req.ExpectedDestinationSHA256), actual)
	}
	return nil
}

func hashRemoteFile(client *sftp.Client, remotePath string) (string, error) {
	file, err := client.Open(remotePath)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func createTransferTemp(client *sftp.Client, destinationPath string) (string, *sftp.File, error) {
	for attempt := 0; attempt < 4; attempt++ {
		random := make([]byte, 12)
		if _, err := rand.Read(random); err != nil {
			return "", nil, err
		}
		tempPath := path.Join(path.Dir(destinationPath), ".opsnerva-transfer-"+hex.EncodeToString(random)+".tmp")
		file, err := client.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err == nil {
			return tempPath, file, nil
		}
		if !os.IsExist(err) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("could not allocate a unique destination temporary file")
}

func transferFailure(parent, transfer context.Context, timeout int, started time.Time, action string, err error) (RawResult, error) {
	result := RawResult{ExitCode: -1, Duration: time.Since(started)}
	if parent.Err() != nil {
		return result, parent.Err()
	}
	if errors.Is(transfer.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("SFTP transfer timed out after %s", time.Duration(timeout)*time.Second)
	}
	return result, fmt.Errorf("%s: %w", action, err)
}
