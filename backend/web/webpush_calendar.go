// File overview: Web Push delivery for calendar reminders, one push per
// reminder, over the per-endpoint cursor the snooze reminders also use.

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

// webPushCalendarIconURL is the calendar area's icon; a reminder is about the
// calendar, not about mail.
const webPushCalendarIconURL = "/icons/calendar.svg"

// webPushKindCalendarReminder names a calendar reminder in the push payload,
// so the service worker renders it with the event's time in the reader's own
// zone rather than as mail.
const webPushKindCalendarReminder = "calendar_reminder"

// calendarReminderPushLimit bounds how many reminders one pass sends to one
// endpoint. More than that pending at once is a backlog, and the pass comes
// straight back for the rest.
const calendarReminderPushLimit = 20

func (s *Server) notifyCalendarReminderWebPushAsync(userID int64) {
	if s == nil || s.store == nil || userID <= 0 {
		return
	}
	s.calendarPushScheduleMu.Lock()
	if s.calendarPushRunning == nil {
		s.calendarPushRunning = map[int64]bool{}
	}
	if s.calendarPushDirty == nil {
		s.calendarPushDirty = map[int64]bool{}
	}
	if s.calendarPushRunning[userID] {
		s.calendarPushDirty[userID] = true
		s.calendarPushScheduleMu.Unlock()
		return
	}
	s.calendarPushRunning[userID] = true
	s.calendarPushScheduleMu.Unlock()
	go s.runScheduledCalendarReminderWebPush(userID)
}

func (s *Server) resumeCalendarReminderWebPushAsync() {
	if s == nil || s.store == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		users, err := s.store.ServiceableUsers(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("calendar reminder web push resume users: %v", err)
			}
			return
		}
		for _, user := range users {
			s.notifyCalendarReminderWebPushAsync(user.ID)
		}
	}()
}

func (s *Server) runScheduledCalendarReminderWebPush(userID int64) {
	retryAttempt := 0
	for {
		s.calendarPushScheduleMu.Lock()
		delete(s.calendarPushDirty, userID)
		s.calendarPushScheduleMu.Unlock()

		retryNeeded := s.notifyCalendarReminderWebPush(context.Background(), userID)
		s.calendarPushScheduleMu.Lock()
		if s.calendarPushDirty[userID] {
			delete(s.calendarPushDirty, userID)
			s.calendarPushScheduleMu.Unlock()
			retryAttempt = 0
			continue
		}
		if retryNeeded && retryAttempt < webPushMaxRetryAttempts {
			retryAttempt++
			s.calendarPushScheduleMu.Unlock()
			timer := time.NewTimer(s.newMailWebPushRetryDelay(retryAttempt))
			<-timer.C
			continue
		}
		delete(s.calendarPushRunning, userID)
		s.calendarPushScheduleMu.Unlock()
		return
	}
}

// notifyCalendarReminderWebPush sends every reminder past each endpoint's
// cursor, one push per reminder so each event gets its own notification, and
// advances the cursor after each accepted push. It reports whether a retry is
// needed.
func (s *Server) notifyCalendarReminderWebPush(ctx context.Context, userID int64) bool {
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
			log.Printf("calendar reminder web push subscriptions user_id=%d: %v", userID, err)
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
		events, count, cursor, err := s.store.CalendarReminderEventsAfter(ctx, userID, sub.LastCalendarReminderEventID, calendarReminderPushLimit)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("calendar reminder web push events user_id=%d subscription_id=%d: %v", userID, sub.ID, err)
			}
			retryNeeded = true
			continue
		}
		if count <= 0 || cursor <= sub.LastCalendarReminderEventID || len(events) == 0 {
			continue
		}
		if count > len(events) {
			// The rest is sent on the next round; this one must not hold the
			// user's other endpoints behind a backlog.
			retryNeeded = true
		}
		for _, event := range events {
			payload, err := json.Marshal(calendarReminderWebPushNotification(event))
			if err != nil {
				log.Printf("calendar reminder web push payload user_id=%d subscription_id=%d: %v", userID, sub.ID, err)
				continue
			}
			result, err := s.sendWebPushNotification(ctx, client, sub, payload, subject)
			if err == nil {
				advanced, advanceErr := s.store.AdvanceWebPushSubscriptionCalendarReminderCursor(ctx, userID, sub, event.ID)
				if advanceErr != nil {
					if ctx.Err() == nil {
						log.Printf("calendar reminder web push cursor user_id=%d subscription_id=%d: %v", userID, sub.ID, advanceErr)
					}
					retryNeeded = true
					break
				}
				if !advanced {
					retryNeeded = true
					break
				}
				continue
			}
			if webPushSubscriptionIsStale(result.StatusCode) {
				deleted, deleteErr := s.store.DeleteWebPushSubscriptionIfCurrent(ctx, userID, sub)
				if deleteErr != nil {
					log.Printf("calendar reminder web push delete stale subscription user_id=%d host=%s: %v", userID, webPushEndpointHost(sub.Endpoint), deleteErr)
					retryNeeded = true
				} else if !deleted {
					retryNeeded = true
				}
				break
			}
			if result.StatusCode >= webPushInvalidStatusMin {
				log.Printf("calendar reminder web push delivery user_id=%d host=%s status=%d", userID, webPushEndpointHost(sub.Endpoint), result.StatusCode)
				if webPushDeliveryIsRetryable(result.StatusCode) {
					retryNeeded = true
				}
				break
			}
			log.Printf("calendar reminder web push delivery user_id=%d host=%s: %v", userID, webPushEndpointHost(sub.Endpoint), err)
			retryNeeded = true
			break
		}
	}
	return retryNeeded
}

// calendarReminderWebPushNotification renders one reminder. The service worker
// prefers the structured fields -- it knows the reader's zone and locale, the
// server does not -- and the title and body here are what a client that only
// knows the two shows.
func calendarReminderWebPushNotification(event store.CalendarReminderEvent) webPushNotification {
	title := truncateWebPushText(event.Summary, 110)
	if title == "" {
		title = "Upcoming event"
	}
	body := calendarReminderLead(event)
	if location := truncateWebPushText(event.Location, 80); location != "" {
		body += " · " + location
	}
	return webPushNotification{
		Title: title, Body: body,
		Tag:  "rolltop-calendar-reminder-" + strconv.FormatInt(event.EventID, 10),
		Icon: webPushCalendarIconURL, Badge: webPushBadgeURL,
		URL:  calendarReminderURL(event),
		Kind: webPushKindCalendarReminder, EventID: event.EventID,
		StartAt: event.StartAt.UTC().Format(time.RFC3339), EndAt: event.EndAt.UTC().Format(time.RFC3339),
		AllDay: event.AllDay, Location: strings.TrimSpace(event.Location), MinutesBefore: event.MinutesBefore,
	}
}

// calendarReminderLead says how far off the event is, from the reminder's own
// offset rather than from the clock: that is the gap the reader set.
func calendarReminderLead(event store.CalendarReminderEvent) string {
	minutes := event.MinutesBefore
	switch {
	case event.AllDay && minutes <= 0:
		return "Today"
	case event.AllDay && minutes <= 24*60:
		return "Tomorrow"
	case event.AllDay:
		return "In " + strconv.Itoa((minutes+24*60-1)/(24*60)) + " days"
	case minutes < 0:
		// A reminder the reader put off past the start.
		return "Started " + strconv.Itoa(-minutes) + " minutes ago"
	case minutes == 0:
		return "Starting now"
	case minutes < 60:
		return "Starts in " + strconv.Itoa(minutes) + " minutes"
	case minutes%(24*60) == 0:
		days := minutes / (24 * 60)
		if days == 1 {
			return "Starts tomorrow"
		}
		return "Starts in " + strconv.Itoa(days) + " days"
	case minutes%60 == 0:
		hours := minutes / 60
		if hours == 1 {
			return "Starts in 1 hour"
		}
		return "Starts in " + strconv.Itoa(hours) + " hours"
	default:
		return "Starts in " + strconv.Itoa(minutes) + " minutes"
	}
}

// calendarReminderURL opens the week the event falls in. The calendar route
// takes any date of the week (weekModel's calendarRouteDate), and the service
// worker recomputes the date in the reader's zone; this is the fallback for a
// client that opens the URL as sent, so it reads the date in the event's own
// zone, which for an all-day event is the UTC date it is stored at.
func calendarReminderURL(event store.CalendarReminderEvent) string {
	at := event.StartAt.UTC()
	if !event.AllDay {
		if zone := strings.TrimSpace(event.TimeZone); zone != "" {
			if loc, err := time.LoadLocation(zone); err == nil {
				at = event.StartAt.In(loc)
			}
		}
	}
	return "/calendar/" + at.Format("2006-01-02")
}
