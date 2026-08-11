package httpapi

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"eino-ops-agent/internal/domain"
	"eino-ops-agent/internal/ids"
)

const maxQueuedChatMessages = 20

var (
	errChatQueueInactive = errors.New("the Agent is not running for this conversation")
	errChatQueueFull     = errors.New("the conversation message queue is full")
)

type queuedChatMessage struct {
	ID          string                  `json:"id"`
	Message     string                  `json:"message"`
	Attachments []domain.ChatAttachment `json:"attachments,omitempty"`
	CreatedAt   time.Time               `json:"created_at"`
}

type chatMessageQueue struct {
	mu       sync.Mutex
	sessions map[string]*chatQueueSession
}

type chatQueueSession struct {
	items     []queuedChatMessage
	ctx       context.Context
	cancel    context.CancelFunc
	accepting bool
}

func newChatMessageQueue() *chatMessageQueue {
	return &chatMessageQueue{sessions: make(map[string]*chatQueueSession)}
}

func (q *chatMessageQueue) begin(sessionID string) (context.Context, bool) {
	sessionID = strings.TrimSpace(sessionID)
	if q == nil || sessionID == "" {
		return nil, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, active := q.sessions[sessionID]; active {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	q.sessions[sessionID] = &chatQueueSession{ctx: ctx, cancel: cancel, accepting: true}
	return ctx, true
}

func (q *chatMessageQueue) enqueue(sessionID, message string, attachments []domain.ChatAttachment) (queuedChatMessage, int, error) {
	if q == nil {
		return queuedChatMessage{}, 0, errChatQueueInactive
	}
	sessionID = strings.TrimSpace(sessionID)
	q.mu.Lock()
	defer q.mu.Unlock()
	state, active := q.sessions[sessionID]
	if !active || !state.accepting {
		return queuedChatMessage{}, 0, errChatQueueInactive
	}
	if len(state.items) >= maxQueuedChatMessages {
		return queuedChatMessage{}, 0, errChatQueueFull
	}
	item := queuedChatMessage{
		ID: ids.New("queue"), Message: strings.TrimSpace(message),
		Attachments: append([]domain.ChatAttachment(nil), attachments...), CreatedAt: time.Now().UTC(),
	}
	state.items = append(state.items, item)
	return publicQueuedChatMessage(item), len(state.items), nil
}

func (q *chatMessageQueue) nextOrFinish(sessionID string) (queuedChatMessage, bool) {
	if q == nil {
		return queuedChatMessage{}, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	sessionID = strings.TrimSpace(sessionID)
	state, active := q.sessions[sessionID]
	if !active || len(state.items) == 0 {
		if active {
			state.accepting = false
		}
		return queuedChatMessage{}, false
	}
	item := state.items[0]
	state.items = state.items[1:]
	return item, true
}

func (q *chatMessageQueue) finish(sessionID string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	sessionID = strings.TrimSpace(sessionID)
	if state := q.sessions[sessionID]; state != nil {
		state.cancel()
	}
	delete(q.sessions, sessionID)
	q.mu.Unlock()
}

func (q *chatMessageQueue) clear(sessionID string) (int, bool) {
	if q == nil {
		return 0, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	sessionID = strings.TrimSpace(sessionID)
	state := q.sessions[sessionID]
	if state == nil {
		return 0, false
	}
	count := len(state.items)
	state.items = nil
	state.accepting = false
	state.cancel()
	return count, true
}

func (q *chatMessageQueue) snapshot(sessionID string) []queuedChatMessage {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	state := q.sessions[strings.TrimSpace(sessionID)]
	if state == nil {
		return nil
	}
	result := make([]queuedChatMessage, len(state.items))
	for index, item := range state.items {
		result[index] = publicQueuedChatMessage(item)
	}
	return result
}

func (q *chatMessageQueue) active(sessionID string) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	_, active := q.sessions[strings.TrimSpace(sessionID)]
	return active
}

func publicQueuedChatMessage(item queuedChatMessage) queuedChatMessage {
	result := item
	result.Attachments = make([]domain.ChatAttachment, len(item.Attachments))
	for index, attachment := range item.Attachments {
		attachment.Data = nil
		result.Attachments[index] = attachment
	}
	return result
}
