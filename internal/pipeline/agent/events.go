package agent

import (
	"sync"
	"time"
)

// flushTimeout bounds how long a closed queue waits for its reader to take
// the remaining events.
const flushTimeout = 5 * time.Second

// eventQueue delivers events in order without ever blocking the sender:
// a slow reader must not stall the agent's output pipe or the ACP
// connection, which closes itself when its own queue overflows.
type eventQueue struct {
	out chan Event

	mu     sync.Mutex
	queue  []Event
	wake   chan struct{}
	closed bool
}

func newEventQueue() *eventQueue {
	q := &eventQueue{out: make(chan Event), wake: make(chan struct{}, 1)}
	go q.pump()
	return q
}

func (q *eventQueue) emit(e Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.queue = append(q.queue, e)
	q.signal()
}

func (q *eventQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) pump() {
	defer close(q.out)
	var deadline <-chan time.Time // set once closed
	for {
		q.mu.Lock()
		closed := q.closed
		if len(q.queue) == 0 {
			q.mu.Unlock()
			if closed {
				return
			}
			<-q.wake
			continue
		}
		e := q.queue[0]
		q.mu.Unlock()
		if closed && deadline == nil {
			deadline = time.After(flushTimeout)
		}
		select {
		case q.out <- e:
			q.mu.Lock()
			q.queue = q.queue[1:]
			q.mu.Unlock()
		case <-q.wake:
		case <-deadline:
			return // nobody reads any more
		}
	}
}

// close stops accepting events; the channel closes once the remaining
// ones are taken (or after flushTimeout).
func (q *eventQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.signal()
}
