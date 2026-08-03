package agent

import (
	"context"
	"sync"
)

type toolExecutionScopeContextKey struct{}

type toolExecutionScope struct {
	ctx      context.Context
	cancel   context.CancelFunc
	onIdle   func()
	mu       sync.Mutex
	active   map[string]int
	modelEnd bool
	idleOnce sync.Once
}

func newToolExecutionScope(parent context.Context, onIdle func()) *toolExecutionScope {
	base := context.WithoutCancel(parent)
	var ctx context.Context
	var cancel context.CancelFunc
	if deadline, ok := parent.Deadline(); ok {
		ctx, cancel = context.WithDeadline(base, deadline)
	} else {
		ctx, cancel = context.WithCancel(base)
	}
	return &toolExecutionScope{ctx: ctx, cancel: cancel, onIdle: onIdle, active: make(map[string]int)}
}

func withToolExecutionScope(ctx context.Context, scope *toolExecutionScope) context.Context {
	if scope == nil {
		return ctx
	}
	return context.WithValue(ctx, toolExecutionScopeContextKey{}, scope)
}

func scopedToolContext(ctx context.Context, toolCallID string) (context.Context, func()) {
	scope, _ := ctx.Value(toolExecutionScopeContextKey{}).(*toolExecutionScope)
	if scope == nil {
		return ctx, func() {}
	}
	scope.started(toolCallID)
	toolCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	stop := context.AfterFunc(scope.ctx, func() { cancel(context.Cause(scope.ctx)) })
	return toolCtx, func() {
		stop()
		cancel(context.Canceled)
		scope.finished(toolCallID)
	}
}

func (s *toolExecutionScope) started(toolCallID string) {
	s.mu.Lock()
	s.active[toolCallID]++
	s.mu.Unlock()
}

func (s *toolExecutionScope) finished(toolCallID string) {
	s.mu.Lock()
	if count := s.active[toolCallID]; count <= 1 {
		delete(s.active, toolCallID)
	} else {
		s.active[toolCallID] = count - 1
	}
	idle := s.modelEnd && len(s.active) == 0
	s.mu.Unlock()
	if idle {
		s.closeIdle()
	}
}

func (s *toolExecutionScope) modelFinished() {
	s.mu.Lock()
	s.modelEnd = true
	idle := len(s.active) == 0
	s.mu.Unlock()
	if idle {
		s.closeIdle()
	}
}

func (s *toolExecutionScope) cancelAll() {
	s.cancel()
}

func (s *toolExecutionScope) activeToolCallIDs() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]struct{}, len(s.active))
	for toolCallID := range s.active {
		result[toolCallID] = struct{}{}
	}
	return result
}

func (s *toolExecutionScope) closeIdle() {
	s.idleOnce.Do(func() {
		s.cancel()
		if s.onIdle != nil {
			s.onIdle()
		}
	})
}
