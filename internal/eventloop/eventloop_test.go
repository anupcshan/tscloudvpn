package eventloop_test

import (
	"sync"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/eventloop"
)

// Mock event and action types for testing
type TestEvent struct {
	ID      int
	Payload string
}

type TestAction struct {
	Type    string
	Details string
}

// TestEventProcessingAndActionExecution verifies the core pump logic
func TestEventProcessingAndActionExecution(t *testing.T) {
	var handledEvents []TestEvent
	var executedActions []TestAction
	var wg sync.WaitGroup
	var mu sync.Mutex // Mutex to protect shared slices

	handler := func(event TestEvent) []TestAction {
		mu.Lock()
		handledEvents = append(handledEvents, event)
		mu.Unlock()
		return []TestAction{
			{Type: "action_for_event", Details: event.Payload},
			{Type: "another_action", Details: event.Payload},
		}
	}

	executor := func(action TestAction) {
		defer wg.Done() // Decrement counter when action is executed
		mu.Lock()
		executedActions = append(executedActions, action)
		mu.Unlock()
	}

	ec := eventloop.NewEventController(handler, executor)
	ec.Start()

	// Send some events
	event1 := TestEvent{ID: 1, Payload: "event1_payload"}
	event2 := TestEvent{ID: 2, Payload: "event2_payload"}

	// Expect 2 actions per event
	wg.Add(2 * 2)

	ec.EnqueueEvent(event1)
	ec.EnqueueEvent(event2)

	// Wait for all actions to be executed or timeout
	waitTimeout(&wg, 5*time.Second, t)

	// Verify handled events
	if len(handledEvents) != 2 {
		t.Errorf("Expected 2 handled events, got %d", len(handledEvents))
	} else {
		if handledEvents[0].ID != event1.ID || handledEvents[1].ID != event2.ID {
			t.Errorf("Handled events mismatch. Got: %+v, %+v", handledEvents[0], handledEvents[1])
		}
	}

	// Verify executed actions
	if len(executedActions) != 4 {
		t.Errorf("Expected 4 executed actions, got %d", len(executedActions))
	}
	// Note: Order of executed actions might not be guaranteed due to concurrent execution.
	// We can check for presence of expected actions.
	expectedActionDetails := map[string]int{"event1_payload": 0, "event2_payload": 0}
	for _, action := range executedActions {
		if action.Type == "action_for_event" || action.Type == "another_action" {
			expectedActionDetails[action.Details]++
		}
	}
	if expectedActionDetails["event1_payload"] != 2 {
		t.Errorf("Expected 2 actions for event1_payload, got %d", expectedActionDetails["event1_payload"])
	}
	if expectedActionDetails["event2_payload"] != 2 {
		t.Errorf("Expected 2 actions for event2_payload, got %d", expectedActionDetails["event2_payload"])
	}

	// Verify closing eventQueue leads to shutdown
	ec.Stop()

	ec.WaitForCompletion()
}

// waitTimeout waits for the waitgroup for the specified duration.
// Returns true if waiting timed out.
func waitTimeout(wg *sync.WaitGroup, timeout time.Duration, t *testing.T) {
	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()
	select {
	case <-c:
		// Completed normally
	case <-time.After(timeout):
		t.Fatalf("Timed out waiting for actions to complete")
	}
}

// TestActionQueueClosesWhenEventPumpFinishes tests if the actionQueue closes
// after the eventQueue is closed and all events are processed.
func TestActionQueueClosesWhenEventPumpFinishes(t *testing.T) {
	var wg sync.WaitGroup
	handler := func(event TestEvent) []TestAction {
		// Minimal handler
		return []TestAction{{Type: "processed", Details: event.Payload}}
	}
	executor := func(action TestAction) {
		// Minimal executor
		defer wg.Done()
	}

	ec := eventloop.NewEventController(handler, executor)
	ec.Start()

	event1 := TestEvent{ID: 1, Payload: "single_event"}
	wg.Add(1) // One action expected

	ec.EnqueueEvent(event1)
	ec.Stop() // Close event queue immediately after sending one event

	// Wait for the action to be processed
	waitTimeout(&wg, 2*time.Second, t)

	ec.WaitForCompletion()
}
