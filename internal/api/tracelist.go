package api

import (
	"net/http"

	"github.com/algonormative/lookout/internal/store"
)

func (s *Server) handleTraceList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TraceListFilter{
		Service:  q.Get("service"),
		RootName: q.Get("root_name"),
		Cursor:   q.Get("cursor"),
	}
	var err error
	if f.Since, err = parseSince(r, "since"); err == nil {
		f.Until, err = parseSince(r, "until")
	}
	if err == nil {
		f.Limit, err = strictInt(r, "limit", 0)
	}
	if err == nil {
		f.Asc, err = parseOrder(r)
	}
	for _, v := range q["attr"] {
		if err != nil {
			break
		}
		var a store.AttrFilter
		a, err = store.ParseAttr(v)
		f.Attrs = append(f.Attrs, a)
	}
	for _, v := range q["missing_score"] {
		if err != nil {
			break
		}
		var ref store.ScoreRef
		ref, err = store.ParseScoreRef(v)
		f.Missing = append(f.Missing, ref)
	}
	for _, v := range q["has_score"] {
		if err != nil {
			break
		}
		var ref store.ScoreRef
		ref, err = store.ParseScoreRef(v)
		f.Has = append(f.Has, ref)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	page, err := s.store.QueryTraceList(r.Context(), f)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, page)
}
