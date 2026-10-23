package main

import "net/http"

// recount is retained for existing browser fixtures. Entity counts and automatic
// covers are read live, so seeding no longer requires a background job.
func (h *harness) recount(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}
