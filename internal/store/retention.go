package store

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Options tunes storage behaviour beyond plain retention. The zero value
// disables everything.
type Options struct {
	// MaxDBBytes caps the used size of the database (pages in use, i.e.
	// (page_count - freelist_count) * page_size). When exceeded after the
	// retention cleanup, whole traces are deleted oldest-first until usage is
	// under 90% of the cap. 0 disables.
	MaxDBBytes int64
	// ArchiveDir, when set, receives every span before it is deleted (see
	// archiveSpans). Empty disables.
	ArchiveDir string
	// CompressMinBytes: spans whose attributes_json + events_json exceed this
	// get their heavy parts stored compressed (see payload.go). 0 disables.
	CompressMinBytes int
}

const (
	capTargetPct  = 90  // delete until used <= this percent of the cap
	deleteBatch   = 500 // spans per archive+delete round
	capTraceBatch = 5   // traces per round on the cap path (small, so the cap overshoots little)
)

// deleteExpired removes spans that started before cutoff. Without an archive
// it is a single DELETE; with one, spans are archived (and fsynced) in batches
// before each batch is deleted.
func (s *SQLiteStore) deleteExpired(ctx context.Context, cutoff int64) (int64, error) {
	if s.opts.ArchiveDir == "" {
		res, err := s.db.ExecContext(ctx, "DELETE FROM spans WHERE start_time_unix_nano < ?", cutoff)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return n, nil
	}
	var total int64
	for {
		ids, err := s.queryStrings(ctx,
			"SELECT span_id FROM spans WHERE start_time_unix_nano < ? ORDER BY start_time_unix_nano LIMIT ?",
			cutoff, deleteBatch)
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		n, err := s.archiveAndDeleteSpans(ctx, ids)
		total += n
		if err != nil {
			return total, err
		}
	}
}

// usedBytes is the database size excluding free pages (which a delete adds to
// the freelist immediately, unlike the file size which only a VACUUM shrinks).
func (s *SQLiteStore) usedBytes(ctx context.Context) (int64, error) {
	var pages, free, pageSize int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, err
	}
	return (pages - free) * pageSize, nil
}

// enforceCap deletes whole traces, oldest trace start first, until the used
// size is under capTargetPct of Options.MaxDBBytes. Scores are never touched.
// It returns the number of spans deleted.
func (s *SQLiteStore) enforceCap(ctx context.Context) (int64, error) {
	if s.opts.MaxDBBytes <= 0 {
		return 0, nil
	}
	used, err := s.usedBytes(ctx)
	if err != nil {
		return 0, err
	}
	if used <= s.opts.MaxDBBytes {
		return 0, nil
	}
	target := s.opts.MaxDBBytes / 100 * capTargetPct
	log.Printf("storage cap: used %d MB > cap %d MB, deleting oldest traces to reach %d MB",
		used>>20, s.opts.MaxDBBytes>>20, target>>20)

	var total int64
	for used > target {
		traces, err := s.oldestTraces(ctx, capTraceBatch)
		if err != nil {
			return total, err
		}
		if len(traces) == 0 {
			break
		}
		ids, err := s.queryStrings(ctx,
			"SELECT span_id FROM spans WHERE trace_id IN ("+placeholders(len(traces))+")", toAny(traces)...)
		if err != nil {
			return total, err
		}
		n, err := s.archiveAndDeleteSpans(ctx, ids)
		total += n
		if err != nil {
			return total, err
		}
		if used, err = s.usedBytes(ctx); err != nil {
			return total, err
		}
	}
	log.Printf("storage cap: deleted %d spans, used now %d MB", total, used>>20)
	return total, nil
}

// oldestTraces returns up to n trace ids ordered by trace start. Walking spans
// by start time and keeping first sightings yields exactly that order, using
// the start_time index. It may return fewer than n (but at least one while any
// span exists) when traces have many spans.
func (s *SQLiteStore) oldestTraces(ctx context.Context, n int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT trace_id FROM spans ORDER BY start_time_unix_nano LIMIT ?", n*40)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
			if len(out) == n {
				break
			}
		}
	}
	return out, rows.Err()
}

// archiveAndDeleteSpans archives the spans (when configured) and only then
// deletes them, in chunks. A failed archive write aborts before the delete of
// that chunk.
func (s *SQLiteStore) archiveAndDeleteSpans(ctx context.Context, ids []string) (int64, error) {
	var total int64
	for len(ids) > 0 {
		chunk := ids
		if len(chunk) > deleteBatch {
			chunk = ids[:deleteBatch]
		}
		ids = ids[len(chunk):]

		if s.opts.ArchiveDir != "" {
			spans, err := s.querySpanRows(ctx, fmt.Sprintf("SELECT %s FROM spans WHERE span_id IN (%s)",
				spanColumns, placeholders(len(chunk))), toAny(chunk)...)
			if err != nil {
				return total, fmt.Errorf("archive read: %w", err)
			}
			if err := archiveSpans(s.opts.ArchiveDir, spans); err != nil {
				return total, fmt.Errorf("archive write (nothing deleted): %w", err)
			}
		}
		res, err := s.db.ExecContext(ctx, "DELETE FROM spans WHERE span_id IN ("+placeholders(len(chunk))+")", toAny(chunk)...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

func (s *SQLiteStore) queryStrings(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ArchiveFileName is the archive file a span starting at the given time
// belongs to (UTC day).
func ArchiveFileName(startNano uint64) string {
	return "spans-" + time.Unix(0, int64(startNano)).UTC().Format("2006-01-02") + ".jsonl.gz"
}

// archiveSpans appends spans to per-day files in dir. Each call writes one
// complete gzip member per day file (concatenated members form a valid gzip
// stream), then fsyncs the file, so when it returns nil the data is durable.
// Line format: one JSON object per span, exactly as the API returns it, with
// attributes_json / events_json fully expanded.
func archiveSpans(dir string, spans []SpanRecord) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	byDay := map[string][]SpanRecord{}
	for _, sp := range spans {
		name := ArchiveFileName(sp.StartTimeUnixNano)
		byDay[name] = append(byDay[name], sp)
	}
	for name, group := range byDay {
		if err := appendArchive(filepath.Join(dir, name), group); err != nil {
			return err
		}
	}
	// Make new directory entries durable too.
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func appendArchive(path string, spans []SpanRecord) (err error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	// A failed write must not leave a half gzip member that would corrupt the
	// members appended after it: cut the file back to its previous length.
	var before int64
	if st, serr := f.Stat(); serr == nil {
		before = st.Size()
	}
	defer func() {
		if err != nil {
			f.Truncate(before)
			f.Close()
		}
	}()
	gz := gzip.NewWriter(f)
	enc := json.NewEncoder(gz)
	enc.SetEscapeHTML(false)
	for i := range spans {
		if err = enc.Encode(&spans[i]); err != nil {
			return err
		}
	}
	if err = gz.Close(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return f.Close()
}
