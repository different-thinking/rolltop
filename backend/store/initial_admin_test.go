// File overview: Creating the first admin account. /setup is reachable without
// a session -- it is the route that hands out the first one -- so the check for
// "are there users yet" and the insert have to be one indivisible step, or two
// requests arriving together both find an empty table and both create an admin.

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestCreateInitialAdminIfNoneCreatesTheFirstAdmin(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	user, err := db.CreateInitialAdminIfNone(ctx, "First@Example.test", "First", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if !user.IsAdmin {
		t.Fatal("the initial account was not created as an admin")
	}
	count, err := db.CountUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("users = %d, want 1", count)
	}
}

func TestCreateInitialAdminIfNoneRefusesOnceAUserExists(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Not an admin: any user at all means the install has been set up, so a
	// second run of setup must not be able to add an admin beside them.
	if _, err := db.CreateUser(ctx, "member@example.test", "Member", "hash", false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateInitialAdminIfNone(ctx, "second@example.test", "Second", "hash"); !errors.Is(err, ErrSetupAlreadyComplete) {
		t.Fatalf("error = %v, want ErrSetupAlreadyComplete", err)
	}
	count, err := db.CountUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("users = %d, want the setup call to have added none", count)
	}
}

// The race this is about: two unauthenticated POSTs to /setup landing together
// on a fresh install. Exactly one may end up with an account.
func TestCreateInitialAdminIfNoneAdmitsOnlyOneConcurrentSetup(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Each attempt asks for a different address on purpose. Sharing one would
	// let the unique index on users.email refuse the second insert, and the
	// test would pass on that rather than on the serialization it is about --
	// two people racing to claim a fresh instance do not pick the same address.
	const attempts = 8
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	results := make([]error, attempts)
	for i := range attempts {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			email := fmt.Sprintf("admin%d@example.test", i)
			_, err := db.CreateInitialAdminIfNone(ctx, email, "Admin", "hash")
			results[i] = err
		}()
	}
	start.Done()
	done.Wait()

	created := 0
	for i, err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrSetupAlreadyComplete):
		default:
			t.Fatalf("attempt %d failed with an unexpected error: %v", i, err)
		}
	}
	if created != 1 {
		t.Fatalf("successful setups = %d, want exactly 1", created)
	}
	count, err := db.CountUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("users = %d, want 1", count)
	}
}
