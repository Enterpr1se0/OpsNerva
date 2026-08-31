package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
)

const currentDateTimePrefix = "Current time: "

type currentDateTimeMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
	now func() time.Time
}

func newCurrentDateTimeMiddleware(now func() time.Time) adk.ChatModelAgentMiddleware {
	return &currentDateTimeMiddleware{
		BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		now:                          now,
	}
}

func (m *currentDateTimeMiddleware) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	if runCtx == nil {
		return ctx, nil, fmt.Errorf("inject current date and time: Agent context is nil")
	}
	current := m.now().Format("2006-01-02 15:04 -07:00")
	runCtx.Instruction = strings.TrimRight(runCtx.Instruction, "\n") + "\n\n" + currentDateTimePrefix + current
	return ctx, runCtx, nil
}
