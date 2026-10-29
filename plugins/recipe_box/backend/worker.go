// File overview: The background loop that runs scan, fetch and file for every
// reader who switched the box on.

package main

import (
	"context"
	"database/sql"
	"log"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

const (
	// idleInterval is how long the loop sleeps with nothing to do. New mail
	// wakes it sooner through the stored-message hook; this is for the links
	// whose retry came due and for a hook that never fired.
	idleInterval = 2 * time.Minute
	// busyPause is the breath between two rounds while a backfill is running,
	// so the walk never becomes a tight loop against the database the reader's
	// own requests are using.
	busyPause = 2 * time.Second
)

func (p *recipeBoxBackend) loop(ctx context.Context, host plugins.BackendHost) {
	// A start is often the server starting: give the sync and the first page
	// loads the database before the box asks anything of it.
	if _, ok := sleep(ctx, 20*time.Second, p.wake); !ok {
		return
	}
	var busy []int64
	lastSweep := time.Time{}
	for {
		// A busy round revisits only the readers that still have work: a
		// backfill keeps the loop on a two-second pace, and asking every
		// reader of the installation for their settings at that pace is a
		// cost the readers who never switched the box on would be paying.
		// Everyone is asked again on a wake-up and once per idle interval.
		if busy == nil || time.Since(lastSweep) >= idleInterval {
			busy = p.round(ctx, host, nil)
			lastSweep = time.Now()
		} else {
			busy = p.round(ctx, host, busy)
		}
		wait := idleInterval
		if len(busy) > 0 {
			wait = busyPause
		} else {
			busy = nil
		}
		woke, ok := sleep(ctx, wait, p.wake)
		if !ok {
			return
		}
		if woke {
			busy = nil
		}
	}
}

// sleep waits for the duration or a wake-up. It reports whether a wake-up
// ended it, and false as its second value when the context ended instead.
func sleep(ctx context.Context, d time.Duration, wake <-chan struct{}) (bool, bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, false
	case <-timer.C:
		return false, true
	case <-wake:
		// A burst of new mail sends a burst of wake-ups; one round answers all
		// of them.
		_, ok := sleep(ctx, time.Second, nil)
		return true, ok
	}
}

// round gives every enabled reader one turn -- or, given a list, only those
// readers -- and returns the readers that have more work waiting.
func (p *recipeBoxBackend) round(ctx context.Context, host plugins.BackendHost, only []int64) []int64 {
	userIDs := only
	if userIDs == nil {
		st, ok := host.Store().(*store.Store)
		if !ok || st == nil {
			return nil
		}
		listed, err := st.ListUserIDsWithAccounts(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("recipe box: list users failed error_type=%T", err)
			}
			return nil
		}
		userIDs = listed
	}
	var busy []int64
	for _, userID := range userIDs {
		if ctx.Err() != nil {
			return nil
		}
		userMore, err := p.runUser(ctx, host, userID)
		if err != nil && ctx.Err() == nil {
			log.Printf("recipe box: pass failed user_id=%d error_type=%T error=%v", userID, err, err)
		}
		if userMore {
			busy = append(busy, userID)
		}
	}
	return busy
}

// runUser is one reader's turn: scan, then fetch, then file. Each step is
// bounded, so a reader in the middle of a backfill takes a slice of the round
// and the rest of it comes on the next one.
func (p *recipeBoxBackend) runUser(ctx context.Context, host plugins.BackendHost, userID int64) (bool, error) {
	db, err := userDB(ctx, host, userID)
	if err != nil {
		return false, err
	}
	settings, err := loadSettings(ctx, db, userID)
	if err != nil || !settings.Enabled {
		return false, err
	}
	_, scanMore, err := scanUser(ctx, db, userID, settings, time.Now().Add(-p.scanSettle).Unix())
	if err != nil {
		return false, err
	}
	resolved, fetchMore, err := p.resolveLinks(ctx, db, userID)
	if err != nil {
		return scanMore, err
	}
	if err := p.fillMissingImages(ctx, db, userID); err != nil {
		return scanMore || fetchMore, err
	}
	if err := fillMissingKinds(ctx, db, userID); err != nil {
		return scanMore || fetchMore, err
	}
	_, fileMore, err := fileMails(ctx, host, db, userID, settings)
	if resolved > 0 {
		// The cookbook and the folder list both read what this round changed.
		if notifier, ok := host.(plugins.UserChangeHost); ok {
			notifier.NotifyUserChanged(userID)
		}
	}
	return scanMore || fetchMore || fileMore, err
}

// fillMissingImages fetches the pictures of recipes that never had one tried:
// a page the reader filed by hand, whose preview named a picture the request
// that filed it did not wait for.
func (p *recipeBoxBackend) fillMissingImages(ctx context.Context, db *sql.DB, userID int64) error {
	rows, err := db.QueryContext(ctx, `SELECT id FROM plugin_recipe_box_recipes WHERE user_id = ? AND image_status = '' AND dismissed = 0 ORDER BY id LIMIT 5`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	// The pictures are fetched after the read has finished: Next returning
	// false has already given the connection back, so the queries below do not
	// wait on it.
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := p.ensureImage(ctx, db, userID, id); err != nil {
			return err
		}
	}
	return nil
}
