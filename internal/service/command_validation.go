package service

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"eino-ops-agent/internal/domain"

	"mvdan.cc/sh/v3/syntax"
)

// containsShellProgram detects direct program invocation without classifying
// the operation. It exists only to enforce managed sudo semantics.
func containsShellProgram(req domain.ExecRequest, name string) (bool, error) {
	if req.Mode == domain.ExecProgram {
		return path.Base(strings.TrimSpace(req.Program)) == name, nil
	}
	if req.Mode != domain.ExecScript && req.Mode != domain.ExecWorkspaceShell {
		return false, nil
	}
	if strings.TrimSpace(req.Script) == "" {
		return false, fmt.Errorf("script is empty")
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(req.Script), "operation.sh")
	if err != nil {
		return false, err
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		var printed bytes.Buffer
		if err := syntax.NewPrinter().Print(&printed, call.Args[0]); err == nil && path.Base(strings.Trim(printed.String(), "'\"")) == name {
			found = true
			return false
		}
		return true
	})
	return found, nil
}
