package main

import "net/http"

// recount runs the groups' count and automatic cover job now.
func (h *harness) recount(w http.ResponseWriter, r *http.Request) {
	if err := h.groups.Recount(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
