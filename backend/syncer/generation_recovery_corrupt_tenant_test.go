// File overview: The generation-recovery gate against a tenant database that
// has stopped answering. Sweeps drop such a tenant from their results, and
// "absent from the pending list" is what normally proves a rebuild finished --
// so the gate has to tell the two apart.

package syncer

import (
	"context"
	"path/filepath"
	"testing"

	"rolltop/backend/store"
)

// gateRunnerWithTenant returns a runner whose store holds one tenant, already
// gated for mailbox generation recovery.
func gateRunnerWithTenant(t *testing.T) (*Runner, *store.Store, store.User) {
	t.Helper()
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	db, err := store.OpenServer(filepath.Join(dataDir, "rolltop.db"), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	user, err := db.CreateUser(ctx, "gated@example.test", "Gated", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunnerWithContext(ctx, &Service{Store: db})
	runner.mu.Lock()
	runner.ensureGenerationRecoveryMapsLocked()
	runner.activateGenerationRecoveryLocked(user.ID)
	runner.mu.Unlock()
	if !runner.generationRecoveryActiveForTest(user.ID) {
		t.Fatal("tenant was not gated for generation recovery")
	}
	return runner, db, user
}

func TestGenerationRecoveryGateHoldsForATenantTheSweepCouldNotRead(t *testing.T) {
	runner, db, user := gateRunnerWithTenant(t)
	snapshot := runner.generationRecoveryEpochSnapshot()
	if err := db.MarkCorrupt(user.ID, "database disk image is malformed"); err == nil {
		t.Fatal("MarkCorrupt did not report the tenant as corrupt")
	}

	// A latched tenant is filtered out of ServiceableUsers, so the rebuild sweep
	// reports it as having nothing pending -- which is indistinguishable from
	// "finished" unless the gate checks why the tenant is missing.
	runner.reconcileGenerationRecoveryUsers(map[int64]bool{},
		map[int64]map[int64]bool{}, map[int64]map[string]bool{}, snapshot)

	if !runner.generationRecoveryActiveForTest(user.ID) {
		t.Fatal("recovery gate cleared for a tenant whose database could not be read")
	}
}

func TestGenerationRecoveryGateStillClearsForAHealthyTenant(t *testing.T) {
	runner, _, user := gateRunnerWithTenant(t)
	snapshot := runner.generationRecoveryEpochSnapshot()

	// The ordinary path has to keep working: a readable tenant absent from the
	// pending list really has finished its rebuilds.
	runner.reconcileGenerationRecoveryUsers(map[int64]bool{},
		map[int64]map[int64]bool{}, map[int64]map[string]bool{}, snapshot)

	if runner.generationRecoveryActiveForTest(user.ID) {
		t.Fatal("recovery gate held for a healthy tenant with no pending rebuilds")
	}
}

// generationRecoveryActiveForTest reports whether the tenant still holds the
// recovery gate itself. MailboxGenerationRecoveryActive also covers the replay
// that clearing schedules, which stays pending in these tests because no
// fetcher is wired up.
func (r *Runner) generationRecoveryActiveForTest(userID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generationRecoveryUsers[userID]
}
