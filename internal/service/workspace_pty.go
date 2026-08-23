package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	ptylib "github.com/aymanbagabas/go-pty"

	"github.com/Enterpr1se0/opsnerva/internal/sshx"
)

type workspacePTYSession struct {
	terminal  ptylib.Pty
	command   *ptylib.Cmd
	done      chan sshx.ShellExit
	readDone  chan struct{}
	drainExit bool
	closeOnce sync.Once
}

var _ sshx.ShellSession = (*workspacePTYSession)(nil)

func startWorkspacePTY(
	ctx context.Context,
	program string,
	args []string,
	directory string,
	environment []string,
	cols int,
	rows int,
	output func(string, []byte),
) (*workspacePTYSession, error) {
	terminal, err := ptylib.New()
	if err != nil {
		return nil, fmt.Errorf("create Workspace PTY: %w", err)
	}
	if err := terminal.Resize(cols, rows); err != nil {
		_ = terminal.Close()
		return nil, fmt.Errorf("resize Workspace PTY: %w", err)
	}
	command := terminal.CommandContext(ctx, program, args...)
	command.Dir = directory
	command.Env = environment
	if err := command.Start(); err != nil {
		_ = terminal.Close()
		return nil, fmt.Errorf("start Workspace PTY: %w", err)
	}
	session := &workspacePTYSession{
		terminal: terminal,
		command:  command,
		done:     make(chan sshx.ShellExit, 1),
		readDone: make(chan struct{}),
	}
	if unixPTY, ok := terminal.(ptylib.UnixPty); ok {
		// The parent must release its slave descriptor so the master reaches
		// EOF after the child exits and all buffered output has been read.
		_ = unixPTY.Slave().Close()
		session.drainExit = true
	}
	go session.copyOutput(output)
	go session.wait(ctx)
	return session, nil
}

func (s *workspacePTYSession) copyOutput(output func(string, []byte)) {
	defer close(s.readDone)
	buffer := make([]byte, 32<<10)
	for {
		count, err := s.terminal.Read(buffer)
		if count > 0 {
			output("stdout", append([]byte(nil), buffer[:count]...))
		}
		if err != nil {
			return
		}
	}
}

func (s *workspacePTYSession) wait(ctx context.Context) {
	err := s.command.Wait()
	if s.drainExit {
		<-s.readDone
	}
	var exitCode *int
	if state := s.command.ProcessState; state != nil {
		code := state.ExitCode()
		if code >= 0 {
			exitCode = &code
		}
	}
	if err == nil && exitCode == nil {
		code := 0
		exitCode = &code
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	s.done <- sshx.ShellExit{ExitCode: exitCode, Err: err}
}

func (s *workspacePTYSession) Write(data []byte) (int, error) {
	return s.terminal.Write(data)
}

func (s *workspacePTYSession) Resize(cols, rows int) error {
	return s.terminal.Resize(cols, rows)
}

func (s *workspacePTYSession) Interrupt() error {
	_, err := s.terminal.Write([]byte{3})
	return err
}

func (s *workspacePTYSession) Wait() sshx.ShellExit {
	return <-s.done
}

func (s *workspacePTYSession) Close() error {
	var result error
	s.closeOnce.Do(func() {
		if s.command.Process != nil {
			_ = s.command.Process.Kill()
		}
		result = s.terminal.Close()
	})
	if errors.Is(result, io.EOF) || errors.Is(result, os.ErrClosed) {
		return nil
	}
	return result
}
