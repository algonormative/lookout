package api

import (
	"net/http"

	"github.com/algonormative/lookout/internal/store"
)

// handleSearch serves GET /v1/search?q=&service=&since=&until=&limit=&cursor=.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.SearchFilter{Q: q.Get("q"), Service: q.Get("service"), Cursor: q.Get("cursor")}
	var err error
	if f.Since, err = parseSince(r, "since"); err == nil {
		f.Until, err = parseSince(r, "until")
	}
	if err == nil {
		f.Limit, err = strictInt(r, "limit", 0)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	page, err := s.store.SearchTraces(r.Context(), f)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, page)
}
