package service

import (
	"context"
	"sync"

	"github.com/Enterpr1se0/opsnerva/internal/domain"
	"github.com/Enterpr1se0/opsnerva/internal/store"
)

const taskEventBuffer = 256

type taskSubscriber struct {
	id      uint64
	taskIDs []string
	events  chan domain.TaskEvent
	done    chan struct{}
	once    sync.Once
}

func taskSnapshot(state *taskState) domain.TaskSnapshot {
	return domain.TaskSnapshot{Task: state.task, Result: state.result, Error: state.err}
}

func (s *Service) SubscribeTaskEvents(ctx context.Context, sessionID string, taskIDs []string) (map[string]domain.TaskSnapshot, <-chan domain.TaskEvent, func()) {
	subscriber := &taskSubscriber{events: make(chan domain.TaskEvent, taskEventBuffer), done: make(chan struct{})}
	snapshots := make(map[string]domain.TaskSnapshot, len(taskIDs))
	missing := make([]string, 0, len(taskIDs))

	s.taskMu.Lock()
	s.taskSubscriberID++
	subscriber.id = s.taskSubscriberID
	for _, taskID := range taskIDs {
		state := s.tasks[taskID]
		if state == nil {
			missing = append(missing, taskID)
			continue
		}
		if sessionID != "" && state.task.SessionID != sessionID {
			snapshots[taskID] = domain.TaskSnapshot{Error: store.ErrNotFound.Error()}
			continue
		}
		snapshots[taskID] = taskSnapshot(state)
		byTask := s.taskSubscribers[taskID]
		if byTask == nil {
			byTask = make(map[uint64]*taskSubscriber)
			s.taskSubscribers[taskID] = byTask
		}
		byTask[subscriber.id] = subscriber
		subscriber.taskIDs = append(subscriber.taskIDs, taskID)
	}
	s.taskMu.Unlock()

	for _, taskID := range missing {
		task, result, taskErr, err := s.store.GetTask(ctx, taskID)
		if err != nil || (sessionID != "" && task.SessionID != sessionID) {
			snapshots[taskID] = domain.TaskSnapshot{Error: store.ErrNotFound.Error()}
			continue
		}
		snapshots[taskID] = domain.TaskSnapshot{Task: task, Result: result, Error: taskErr}
	}

	cancel := func() {
		subscriber.once.Do(func() {
			s.taskMu.Lock()
			for _, taskID := range subscriber.taskIDs {
				if byTask := s.taskSubscribers[taskID]; byTask != nil {
					delete(byTask, subscriber.id)
					if len(byTask) == 0 {
						delete(s.taskSubscribers, taskID)
					}
				}
			}
			s.taskMu.Unlock()
			close(subscriber.done)
		})
	}
	return snapshots, subscriber.events, cancel
}

func (s *Service) publishTaskEvent(event domain.TaskEvent) {
	if event.TaskID == "" {
		return
	}
	s.taskMu.RLock()
	subscribers := make([]*taskSubscriber, 0, len(s.taskSubscribers[event.TaskID]))
	for _, subscriber := range s.taskSubscribers[event.TaskID] {
		subscribers = append(subscribers, subscriber)
	}
	s.taskMu.RUnlock()
	for _, subscriber := range subscribers {
		select {
		case <-subscriber.done:
			continue
		default:
		}
		select {
		case subscriber.events <- event:
		default:
			select {
			case <-subscriber.events:
			default:
			}
			select {
			case subscriber.events <- domain.TaskEvent{Type: "resync"}:
			default:
			}
		}
	}
}

func notifyTaskWaitersLocked(state *taskState) {
	if state.notify != nil {
		close(state.notify)
	}
	state.notify = make(chan struct{})
}
