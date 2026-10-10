package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockPublisher struct {
	mu        sync.Mutex
	err       error
	published []domain.Event
}

func (m *mockPublisher) Publish(_ context.Context, e domain.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.published = append(m.published, e)
	return nil
}

func TestDispatchPublisher_FailedPersistSkipsHooks(t *testing.T) {
	mockErr := errors.New("database connection failed")
	mockInner := &mockPublisher{err: mockErr}
	dispatcher := NewDispatchPublisher(mockInner)

	var hookCalled bool
	var mu sync.Mutex
	dispatcher.Subscribe(func(e domain.Event) {
		mu.Lock()
		defer mu.Unlock()
		hookCalled = true
	})

	err := dispatcher.Publish(context.Background(), domain.Event{
		PropertyID: uuid.New(),
		EventType:  "test.event",
	})

	if !errors.Is(err, mockErr) {
		t.Fatalf("expected error %v, got %v", mockErr, err)
	}

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if hookCalled {
		t.Fatal("hook should not be called when inner persist fails")
	}
}

func TestDispatchPublisher_SuccessNotifiesAllSubscribers(t *testing.T) {
	mockInner := &mockPublisher{}
	dispatcher := NewDispatchPublisher(mockInner)

	var wg sync.WaitGroup
	wg.Add(2)

	var received1, received2 domain.Event
	dispatcher.Subscribe(func(e domain.Event) {
		received1 = e
		wg.Done()
	})
	dispatcher.Subscribe(func(e domain.Event) {
		received2 = e
		wg.Done()
	})

	event := domain.Event{
		PropertyID: uuid.New(),
		EventType:  "capital.added",
		OccurredAt: time.Now().UTC(),
	}

	err := dispatcher.Publish(context.Background(), event)
	if err != nil {
		t.Fatalf("unexpected publish error: %v", err)
	}

	wg.Wait()

	if received1.EventType != event.EventType || received2.EventType != event.EventType {
		t.Fatalf("subscribers did not receive event correctly: %+v, %+v", received1, received2)
	}
}

func TestDispatchPublisher_HookPanicRecovered(t *testing.T) {
	mockInner := &mockPublisher{}
	dispatcher := NewDispatchPublisher(mockInner)

	var wg sync.WaitGroup
	wg.Add(1)

	// Panicking hook
	dispatcher.Subscribe(func(e domain.Event) {
		panic("hook failure deliberate panic")
	})

	// Healthy hook
	var normalCalled bool
	dispatcher.Subscribe(func(e domain.Event) {
		normalCalled = true
		wg.Done()
	})

	err := dispatcher.Publish(context.Background(), domain.Event{
		PropertyID: uuid.New(),
		EventType:  "test.panic_recovery",
	})
	if err != nil {
		t.Fatalf("unexpected publish error: %v", err)
	}

	wg.Wait()

	if !normalCalled {
		t.Fatal("expected healthy subscriber to complete despite other hook panic")
	}
}
