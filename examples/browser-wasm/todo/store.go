// Package todo is the service this example builds a UI against: an ordinary
// gRPC implementation of todopb.TodoService and the in-memory store behind it.
// Nothing here is browser-specific — it is the code a server process would
// run, and GOOS=js GOARCH=wasm is the only difference. That is the whole claim
// the example makes.
package todo

import (
	"sync"

	"github.com/lesomnus/grpc-dgram/examples/browser-wasm/todopb"
)

// Store is the seam.
//
// Handlers, validation, statuses and streaming follow the service into the
// browser unchanged; storage does not — a page has no socket to a database and
// no file to open. So the one part a real project must reimplement for the
// in-page build (stub it, or back it with IndexedDB) is named by an interface,
// and everything on the other side of it stays the real thing in both builds.
//
// Nothing here returns an error: which status a bad request draws is the
// handler's business (see service.go), not the storage's.
type Store interface {
	List() []*todopb.Task
	Add(title string) *todopb.Task
	// Toggle flips a task's done flag. ok is false when there is no such task.
	Toggle(id uint32) (task *todopb.Task, ok bool)
	// Remove deletes a task. ok is false when there is no such task.
	Remove(id uint32) (task *todopb.Task, ok bool)
	// Watch returns every mutation from this moment on, and the function that
	// unsubscribes. The channel is closed when the subscription ends — either
	// through that function or because the subscriber fell too far behind (see
	// MemStore.emit) — so a reader can range over it.
	Watch() (<-chan *todopb.Event, func())
}

// watchBuffer is how many events a watcher may lag by. The page consumes them
// as fast as its Watch stream delivers, so this only ever absorbs a burst.
const watchBuffer = 64

// MemStore is the only Store this example ships: tasks in a slice, watchers in
// a set, one mutex over both. It is deliberately unremarkable — the interesting
// half of the example is that the service above it runs in two places.
type MemStore struct {
	mu     sync.Mutex
	nextID uint32
	tasks  []*todopb.Task
	subs   map[*watcher]struct{}
}

type watcher struct {
	ch     chan *todopb.Event
	closed bool
}

// NewMemStore returns a store holding one task per title, so a fresh instance
// has something to show. Each browser reload starts a new instance and
// therefore starts from exactly this state again.
func NewMemStore(titles ...string) *MemStore {
	s := &MemStore{subs: map[*watcher]struct{}{}}
	for _, title := range titles {
		s.nextID++
		s.tasks = append(s.tasks, &todopb.Task{Id: s.nextID, Title: title})
	}
	return s
}

func (s *MemStore) List() []*todopb.Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*todopb.Task, len(s.tasks))
	for i, t := range s.tasks {
		out[i] = clone(t)
	}
	return out
}

func (s *MemStore) Add(title string) *todopb.Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	t := &todopb.Task{Id: s.nextID, Title: title}
	s.tasks = append(s.tasks, t)
	return s.emit(todopb.Event_KIND_ADDED, t)
}

func (s *MemStore) Toggle(id uint32) (*todopb.Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.indexOf(id)
	if i < 0 {
		return nil, false
	}
	t := s.tasks[i]
	t.Done = !t.Done
	return s.emit(todopb.Event_KIND_CHANGED, t), true
}

func (s *MemStore) Remove(id uint32) (*todopb.Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.indexOf(id)
	if i < 0 {
		return nil, false
	}
	t := s.tasks[i]
	s.tasks = append(s.tasks[:i], s.tasks[i+1:]...)
	return s.emit(todopb.Event_KIND_REMOVED, t), true
}

func (s *MemStore) Watch() (<-chan *todopb.Event, func()) {
	w := &watcher{ch: make(chan *todopb.Event, watchBuffer)}

	s.mu.Lock()
	s.subs[w] = struct{}{}
	s.mu.Unlock()

	return w.ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.drop(w)
	}
}

func (s *MemStore) indexOf(id uint32) int {
	for i, t := range s.tasks {
		if t.Id == id {
			return i
		}
	}
	return -1
}

// emit publishes one mutation to every watcher and returns the task as the
// caller should hand it out. Callers hold s.mu.
func (s *MemStore) emit(kind todopb.Event_Kind, t *todopb.Task) *todopb.Task {
	// Copies, not the stored pointer: the response is marshaled after the
	// handler returns and after this lock is released, so sharing the live
	// task would let a later Toggle race the encoder. One Event does serve
	// every watcher — nobody mutates it.
	out := clone(t)
	ev := &todopb.Event{Kind: kind, Task: clone(t)}
	for w := range s.subs {
		select {
		case w.ch <- ev:
		default:
			// A watcher too slow to keep up is dropped, not silently skipped:
			// a subscriber that misses one mutation shows a list that is
			// quietly wrong forever, while a stream that ends with a status
			// says so out loud — the page stops trusting its list and reports
			// the reason (web/main.js).
			s.drop(w)
		}
	}
	return out
}

// drop unsubscribes a watcher and closes its channel, ending the Watch handler
// that reads it. Idempotent, because both the handler's own unsubscribe and a
// fell-behind eviction reach it. Callers hold s.mu.
func (s *MemStore) drop(w *watcher) {
	if _, ok := s.subs[w]; !ok {
		return
	}
	delete(s.subs, w)
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
}

func clone(t *todopb.Task) *todopb.Task {
	return &todopb.Task{Id: t.Id, Title: t.Title, Done: t.Done}
}
