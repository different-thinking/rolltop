// File overview: What the service says about itself. The two backends keep
// their index in places a caller cannot compare, so these answers are what a
// storage page reports instead of guessing from the volume.

package search

import (
	"testing"
	"time"
)

func TestBackendNamesWhereTheIndexLives(t *testing.T) {
	svc, _, _, _ := openPostgresSearchFixtures(t)
	if svc.Backend() != BackendPostgres {
		t.Fatalf("backend = %q, want %q", svc.Backend(), BackendPostgres)
	}
	bleve, err := OpenPerUser(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bleve.Close() })
	if bleve.Backend() != BackendBleve {
		t.Fatalf("backend = %q, want %q", bleve.Backend(), BackendBleve)
	}
	// Bleve builds fuzzy queries from the index it already has; there is no
	// second thing to install and so nothing that can be missing.
	if !bleve.FuzzyAvailable() {
		t.Fatal("Bleve reported fuzzy matching unavailable")
	}
	// A nil service is the no-search install, and every one of these has to
	// answer rather than panic: the storage page calls them unconditionally.
	var absent *Service
	if absent.Backend() != "" || absent.FuzzyAvailable() || absent.MaintenanceTasks(1) != nil {
		t.Fatal("a nil service must answer empty rather than claim a backend")
	}
}

// The trigram index is one index for the whole database, so the tenant whose
// search is answering without typo tolerance has to see it even though the work
// is not theirs. Per-tenant work must stay theirs.
func TestMaintenanceTasksScopeToTheTenantOrTheWholeServer(t *testing.T) {
	svc, _, _, _ := openPostgresSearchFixtures(t)
	started := time.Unix(1700000000, 0)
	doneGlobal := svc.StartMaintenance("search_fuzzy_index", "Building the fuzzy index", 0, started)
	doneOne := svc.StartMaintenance("search_coverage_check", "Checking coverage", 7, started.Add(time.Second))
	doneTwo := svc.StartMaintenance("search_coverage_check", "Checking coverage", 8, started.Add(2*time.Second))

	seven := svc.MaintenanceTasks(7)
	if len(seven) != 2 {
		t.Fatalf("tasks for user 7 = %+v, want the global one and their own", seven)
	}
	if seven[0].Kind != "search_fuzzy_index" || seven[1].UserID != 7 {
		t.Fatalf("tasks are not oldest-first with the right owners: %+v", seven)
	}
	eight := svc.MaintenanceTasks(8)
	if len(eight) != 2 || eight[1].UserID != 8 {
		t.Fatalf("tasks for user 8 = %+v, want the global one and their own", eight)
	}

	doneOne()
	// Finishing twice must not disturb anything: callers defer it and also call
	// it early rather than tracking a flag.
	doneOne()
	if tasks := svc.MaintenanceTasks(7); len(tasks) != 1 || tasks[0].UserID != 0 {
		t.Fatalf("tasks after finishing = %+v, want only the global one", tasks)
	}
	doneGlobal()
	doneTwo()
	if tasks := svc.MaintenanceTasks(8); len(tasks) != 0 {
		t.Fatalf("tasks after everything finished = %+v, want none", tasks)
	}
}
