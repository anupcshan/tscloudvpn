package eventloop

import "sync"

type EventController[EventType any, ActionType any] struct {
	eventQueue   chan EventType
	eventHandler func(event EventType) []ActionType

	actionQueue    chan ActionType
	actionExecutor func(action ActionType)

	actionsCompleted sync.WaitGroup // WaitGroup to indicate when all actions are processed
}

func NewEventController[EventType any, ActionType any](
	handler func(event EventType) []ActionType,
	actionExecutor func(action ActionType),
) *EventController[EventType, ActionType] {
	return &EventController[EventType, ActionType]{
		eventQueue:     make(chan EventType, 100), // Buffered channel for events
		eventHandler:   handler,
		actionQueue:    make(chan ActionType, 100), // Buffered channel for actions
		actionExecutor: actionExecutor,
	}
}

func (ec *EventController[EventType, ActionType]) Start() {
	go func() {
		for event := range ec.eventQueue {
			actions := ec.eventHandler(event)
			for _, action := range actions {
				ec.actionQueue <- action
			}
		}

		close(ec.actionQueue) // Close action queue when event processing is done
	}()

	ec.actionsCompleted.Add(1)
	go func() {
		defer ec.actionsCompleted.Done()

		for action := range ec.actionQueue {
			// Execute each action in a separate goroutine to avoid blocking the action queue.
			// TODO: Consider using a worker pool for better control over concurrency.
			ec.actionsCompleted.Add(1)

			go func() {
				defer ec.actionsCompleted.Done()
				ec.actionExecutor(action)
			}()
		}
	}()
}

func (ec *EventController[EventType, ActionType]) EnqueueEvent(event EventType) {
	ec.eventQueue <- event
}

func (ec *EventController[EventType, ActionType]) Stop() {
	close(ec.eventQueue)
}

func (ec *EventController[EventType, ActionType]) WaitForCompletion() {
	ec.actionsCompleted.Wait()
}
