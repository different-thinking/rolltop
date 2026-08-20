// File overview: What the service can say about itself — which backend it runs
// on, whether typo tolerance is answering, and what index maintenance is in
// flight right now.
//
// None of this is needed to search. It exists because the two backends store
// the index in places a reader cannot compare: Bleve leaves a directory on the
// data volume that a storage page can measure by walking it, and PostgreSQL
// leaves rows that only a query can size. A page that walks the volume reports
// zero bytes on the Postgres backend and says nothing about why, so the pages
// ask here instead of guessing from what is on disk.

package search

import (
	"sort"
	"strconv"
	"sync"
	"time"
)

// The backend names are the values ROLLTOP_SEARCH_BACKEND takes, so the string
// an operator set and the string a page shows are the same string.
const (
	BackendBleve    = "bleve"
	BackendPostgres = "postgres"
)

// Backend names where this service keeps its index.
func (s *Service) Backend() string {
	if s == nil {
		return ""
	}
	if s.pg != nil {
		return BackendPostgres
	}
	return BackendBleve
}

// FuzzyAvailable reports whether typo-tolerant matching can answer. Bleve
// builds its fuzzy queries from the index it already has, so it is always able
// to; the Postgres backend needs pg_trgm and its trigram index, which a hoster
// may refuse and which takes time to build on a filled table. Search degrades
// to exact matching until it is there, and a reader who has just been told
// nothing matched deserves to know which of the two they got.
func (s *Service) FuzzyAvailable() bool {
	if s == nil {
		return false
	}
	if s.pg != nil {
		return s.pg.TrigramSearchEnabled()
	}
	return true
}

// MaintenanceTask is one piece of index upkeep that is running now. It is not a
// sync run and not a Bleve writer reservation: those have their own records and
// their own cancel paths. This is for work that happens beside the request path
// with nothing else to report it — the trigram index being built behind the
// listener is minutes of a tenant's search silently answering without typo
// tolerance, and the log line saying so is not somewhere a reader looks.
type MaintenanceTask struct {
	// Key identifies this task for as long as it runs. Unique per service.
	Key string
	// Kind is the machine name, matching the vocabulary the activity view
	// already uses for worker kinds.
	Kind string
	// Label is the sentence a reader sees.
	Label string
	// UserID is the tenant the work belongs to, or 0 when it serves every
	// tenant — the trigram index is one index for the whole database.
	UserID    int64
	StartedAt time.Time
}

// maintenanceRegistry is kept off Service.mu on purpose: that mutex guards the
// index cache and is held across Bleve opens, and a status read must not queue
// behind one.
type maintenanceRegistry struct {
	mu    sync.Mutex
	seq   int64
	tasks map[string]MaintenanceTask
}

// StartMaintenance records work that has begun and returns the function that
// records it finished. The returned function is safe to call more than once, so
// a caller may defer it and also call it early without keeping a flag.
func (s *Service) StartMaintenance(kind, label string, userID int64, startedAt time.Time) func() {
	if s == nil {
		return func() {}
	}
	s.maintenance.mu.Lock()
	s.maintenance.seq++
	key := kind + ":" + strconv.FormatInt(s.maintenance.seq, 10)
	if s.maintenance.tasks == nil {
		s.maintenance.tasks = make(map[string]MaintenanceTask)
	}
	s.maintenance.tasks[key] = MaintenanceTask{
		Key:       key,
		Kind:      kind,
		Label:     label,
		UserID:    userID,
		StartedAt: startedAt,
	}
	s.maintenance.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.maintenance.mu.Lock()
			delete(s.maintenance.tasks, key)
			s.maintenance.mu.Unlock()
		})
	}
}

// MaintenanceTasks lists what is running for one tenant: their own work, plus
// the whole-database work that decides what their search can do. Oldest first,
// so a list that grows does not reshuffle what is already on screen.
func (s *Service) MaintenanceTasks(userID int64) []MaintenanceTask {
	if s == nil {
		return nil
	}
	s.maintenance.mu.Lock()
	out := make([]MaintenanceTask, 0, len(s.maintenance.tasks))
	for _, task := range s.maintenance.tasks {
		if task.UserID == 0 || task.UserID == userID {
			out = append(out, task)
		}
	}
	s.maintenance.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].Key < out[j].Key
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}
