// File overview: Authenticated, cursor-based feed of calendar reminders that
// came due, for a browser without Web Push and for native clients.

package web

import (
	"net/http"
	"strconv"
	"strings"

	"rolltop/backend/store"
)

type apiCalendarReminderNotification struct {
	ID            int64  `json:"id"`
	EventID       int64  `json:"event_id"`
	CalendarID    int64  `json:"calendar_id"`
	Summary       string `json:"summary"`
	Location      string `json:"location"`
	StartAt       string `json:"start_at"`
	EndAt         string `json:"end_at"`
	AllDay        bool   `json:"all_day"`
	MinutesBefore int    `json:"minutes_before"`
	DueAt         string `json:"due_at"`
	// URL opens the week of the event; Title and Body are what a client that
	// does not render the fields itself shows.
	URL   string `json:"url"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

type apiCalendarReminderNotificationsResponse struct {
	UserID    int64                             `json:"user_id"`
	Count     int                               `json:"count"`
	Cursor    int64                             `json:"cursor"`
	Reminders []apiCalendarReminderNotification `json:"reminders"`
}

func (s *Server) apiCalendarReminderNotifications(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	rawAfter := strings.TrimSpace(r.URL.Query().Get("after"))
	if rawAfter == "" {
		cursor, err := s.store.LatestCalendarReminderEventID(r.Context(), cu.User.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, apiCalendarReminderNotificationsResponse{
			UserID: cu.User.ID, Cursor: cursor, Reminders: []apiCalendarReminderNotification{},
		})
		return
	}
	after, err := strconv.ParseInt(rawAfter, 10, 64)
	if err != nil || after < 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid notification cursor")
		return
	}
	events, count, cursor, err := s.store.CalendarReminderEventsAfter(r.Context(), cu.User.ID, after, calendarReminderPushLimit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, apiCalendarReminderNotificationsResponse{
		UserID: cu.User.ID, Count: count, Cursor: cursor, Reminders: apiCalendarReminderNotifications(events),
	})
}

func apiCalendarReminderNotifications(events []store.CalendarReminderEvent) []apiCalendarReminderNotification {
	out := make([]apiCalendarReminderNotification, 0, len(events))
	for _, event := range events {
		rendered := calendarReminderWebPushNotification(event)
		out = append(out, apiCalendarReminderNotification{
			ID: event.ID, EventID: event.EventID, CalendarID: event.CalendarID,
			Summary: event.Summary, Location: event.Location,
			StartAt: timeString(event.StartAt), EndAt: timeString(event.EndAt), AllDay: event.AllDay,
			MinutesBefore: event.MinutesBefore, DueAt: timeString(event.DueAt),
			URL: rendered.URL, Title: rendered.Title, Body: rendered.Body,
		})
	}
	return out
}
