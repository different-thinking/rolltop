// File overview: Web Push for a ticket that just arrived, one push per trip,
// over a per-endpoint cursor beside the new-mail, snooze and calendar ones.
//
// Only trips the fetch path marked (trips.notify_at) are sent: a journey still
// to come, from a message sent in the last few days. That is what keeps an
// initial sync or a backfill from announcing every ticket a mailbox ever held.
// The push opens the mail the ticket came in, where the card offers the legs
// for the calendar.

package web

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/store"
)

const webPushTravelIconURL = "/icons/calendar.svg"

func (s *Server) notifyTripWebPushAsync(userID int64) {
	if s == nil || s.store == nil || userID <= 0 {
		return
	}
	s.tripPushScheduleMu.Lock()
	if s.tripPushRunning == nil {
		s.tripPushRunning = map[int64]bool{}
	}
	if s.tripPushDirty == nil {
		s.tripPushDirty = map[int64]bool{}
	}
	if s.tripPushRunning[userID] {
		s.tripPushDirty[userID] = true
		s.tripPushScheduleMu.Unlock()
		return
	}
	s.tripPushRunning[userID] = true
	s.tripPushScheduleMu.Unlock()
	go s.runScheduledTripWebPush(userID)
}

// resumeTripWebPushAsync sends what a restart left undelivered.
func (s *Server) resumeTripWebPushAsync() {
	if s == nil || s.store == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		users, err := s.store.ServiceableUsers(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("trip web push resume users: %v", err)
			}
			return
		}
		for _, user := range users {
			s.notifyTripWebPushAsync(user.ID)
		}
	}()
}

func (s *Server) runScheduledTripWebPush(userID int64) {
	retryAttempt := 0
	for {
		s.tripPushScheduleMu.Lock()
		delete(s.tripPushDirty, userID)
		s.tripPushScheduleMu.Unlock()

		retryNeeded := s.notifyTripWebPush(context.Background(), userID)
		s.tripPushScheduleMu.Lock()
		if s.tripPushDirty[userID] {
			delete(s.tripPushDirty, userID)
			s.tripPushScheduleMu.Unlock()
			retryAttempt = 0
			continue
		}
		if retryNeeded && retryAttempt < webPushMaxRetryAttempts {
			retryAttempt++
			s.tripPushScheduleMu.Unlock()
			timer := time.NewTimer(s.newMailWebPushRetryDelay(retryAttempt))
			<-timer.C
			continue
		}
		delete(s.tripPushRunning, userID)
		s.tripPushScheduleMu.Unlock()
		return
	}
}

// notifyTripWebPush sends every marked trip past each endpoint's cursor and
// reports whether a retry is needed.
func (s *Server) notifyTripWebPush(ctx context.Context, userID int64) bool {
	if s == nil || s.store == nil || userID <= 0 {
		return false
	}
	lock := s.webPushDeliveryLock(userID)
	lock.Lock()
	defer lock.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	subs, err := s.store.ListWebPushSubscriptions(ctx, userID)
	if err != nil {
		if store.IsClosed(err) {
			return false
		}
		if ctx.Err() == nil {
			log.Printf("trip web push subscriptions user_id=%d: %v", userID, err)
		}
		return true
	}
	if len(subs) == 0 {
		return false
	}
	user, userErr := s.store.GetUserByID(ctx, userID)
	subject := ""
	if userErr == nil {
		subject = user.Email
	}
	client := s.webPushClient
	if client == nil {
		client = newWebPushHTTPClient()
	}
	retryNeeded := false
	for _, sub := range subs {
		if ctx.Err() != nil {
			return true
		}
		notices, _, err := s.store.TripNoticesAfter(ctx, userID, sub.LastTripNoticeID)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("trip web push notices user_id=%d subscription_id=%d: %v", userID, sub.ID, err)
			}
			retryNeeded = true
			continue
		}
		if len(notices) >= store.MaxTripNoticesPerPass {
			// The rest is sent on the next round rather than on the next sync.
			retryNeeded = true
		}
		for _, notice := range notices {
			payload, err := json.Marshal(tripWebPushNotification(notice))
			if err != nil {
				continue
			}
			result, err := s.sendWebPushNotification(ctx, client, sub, payload, subject)
			if err == nil {
				advanced, advanceErr := s.store.AdvanceWebPushSubscriptionTripNoticeCursor(ctx, userID, sub, notice.TripID)
				if advanceErr != nil || !advanced {
					if advanceErr != nil && ctx.Err() == nil {
						log.Printf("trip web push cursor user_id=%d subscription_id=%d: %v", userID, sub.ID, advanceErr)
					}
					retryNeeded = true
					break
				}
				continue
			}
			if webPushSubscriptionIsStale(result.StatusCode) {
				deleted, deleteErr := s.store.DeleteWebPushSubscriptionIfCurrent(ctx, userID, sub)
				if deleteErr != nil || !deleted {
					retryNeeded = true
				}
				break
			}
			if result.StatusCode >= webPushInvalidStatusMin {
				log.Printf("trip web push delivery user_id=%d host=%s status=%d", userID, webPushEndpointHost(sub.Endpoint), result.StatusCode)
				if webPushDeliveryIsRetryable(result.StatusCode) {
					retryNeeded = true
				}
				break
			}
			log.Printf("trip web push delivery user_id=%d host=%s: %v", userID, webPushEndpointHost(sub.Endpoint), err)
			retryNeeded = true
			break
		}
	}
	return retryNeeded
}

// tripWebPushNotification renders one trip. The time is written as the ticket
// printed it: it is the departure's own local time, which is what a traveller
// reads off a departure board, and the server does not know the reader's zone
// to convert it into anyway.
func tripWebPushNotification(notice store.TripNotice) webPushNotification {
	what := map[string]string{"flight": "Flight", "train": "Train", "bus": "Coach", "event": "Ticket"}[notice.Kind]
	if what == "" {
		what = "Ticket"
	}
	title := "New " + strings.ToLower(what) + " ticket"
	if notice.Kind == "event" {
		title = "New ticket"
	}
	parts := []string{truncateWebPushText(notice.Title, 80)}
	if notice.From != "" && notice.To != "" {
		parts = append(parts, truncateWebPushText(notice.From+" → "+notice.To, 80))
	} else if notice.Venue != "" {
		parts = append(parts, truncateWebPushText(notice.Venue, 60))
	}
	if when := tripNoticeWhen(notice.Start); when != "" {
		parts = append(parts, when)
	}
	body := strings.Join(nonEmptyStrings(parts), " · ") + " - add it to your calendar?"
	return webPushNotification{
		Title: "rolltop - " + title, Body: body,
		Tag:  "rolltop-trip-" + strconv.FormatInt(notice.TripID, 10),
		Icon: webPushTravelIconURL, Badge: webPushBadgeURL,
		URL:       tripMessageURL(notice.MessageID),
		MessageID: notice.MessageID,
	}
}

func tripNoticeWhen(start string) string {
	if t, err := time.Parse("2006-01-02T15:04", start); err == nil {
		return t.Format("Mon 2 Jan, 15:04")
	}
	if t, err := time.Parse("2006-01-02", start); err == nil {
		return t.Format("Mon 2 Jan")
	}
	return ""
}

func nonEmptyStrings(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}
