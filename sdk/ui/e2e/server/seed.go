package main

import "net/http"

// recount runs the entities' count and automatic cover job now.
func (h *harness) recount(w http.ResponseWriter, r *http.Request) {
	if err := h.entities.Recount(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
