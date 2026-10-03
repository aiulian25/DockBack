package api

import (
	"net/http"
	"strconv"

	"dockback/internal/store"
)

// handleListAlerts returns persisted alerts (F46), newest-first, optionally only
// unacknowledged, paged via ?page (100 per page).
func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	onlyUnacked := q.Get("unacked") == "1" || q.Get("unacked") == "true"
	const pageSize = 100
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 0 {
		page = 0
	}
	alerts, err := s.store.ListAlerts(onlyUnacked, pageSize, page*pageSize)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if alerts == nil {
		alerts = []*store.Alert{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts, "page": page})
}

// handleAlertsCount returns the unacknowledged alert count for the header bell (F46).
func (s *Server) handleAlertsCount(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.CountUnacked()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"unacked": n})
}

// handleAckAlert acknowledges one alert.
func (s *Server) handleAckAlert(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid alert id")
		return
	}
	if err := s.store.AckAlert(id); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "alert.ack", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "acked"})
}

// handleAckAllAlerts acknowledges every alert.
func (s *Server) handleAckAllAlerts(w http.ResponseWriter, r *http.Request) {
	if err := s.store.AckAllAlerts(); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.Audit(userFrom(r), "alert.ack_all", "", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "acked"})
}
