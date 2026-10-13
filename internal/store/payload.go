package store

import (
	"bytes"
	"compress/flate"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

// Large span payloads (LLM prompts and completions, tool output, images) hold
// ~99% of the bytes in a trace database. Spans over Options.CompressMinBytes
// keep a "lite" attributes_json in the spans table and the full attributes and
// events, compressed, in span_payloads. Every read path goes through
// querySpanRows, which puts the full values back, so callers never see the
// split.
//
//   - attributes_json (lite): string values over liteStringBytes are replaced
//     by {"$lookout_ref":"payload","bytes":N}; everything else is unchanged, so
//     json_extract filters on small keys keep working.
//   - events_json: replaced by a one-element sentinel array when the events
//     moved to the payload.
//   - span_payloads.data: flate("flags | [uvarint len, attrs] | [uvarint len, events]"),
//     holding the exact original strings. Only the parts that differ from the
//     columns are stored (flag bit 0 = attrs, bit 1 = events).
//
// Codec is flate from the standard library: no dependency, ~2.4x on real LLM
// payloads. The codec column lets a better codec (zstd) be added later without
// rewriting rows.
const (
	liteStringBytes = 1024
	codecFlate      = "flate"
	payloadRefKey   = "$lookout_ref"
	eventsSentinel  = `[{"$lookout_ref":"payload"}]`

	payloadFlagAttrs  = 1
	payloadFlagEvents = 2

	payloadFetchBatch = 500
)

func (s *SQLiteStore) migratePayloads() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS span_payloads (
			span_id TEXT PRIMARY KEY,
			codec   TEXT NOT NULL,
			data    BLOB NOT NULL
		);
		CREATE TRIGGER IF NOT EXISTS spans_payload_ad AFTER DELETE ON spans BEGIN
			DELETE FROM span_payloads WHERE span_id = old.span_id;
		END;
	`)
	return err
}

var flatePool = sync.Pool{New: func() any {
	w, _ := flate.NewWriter(io.Discard, flate.DefaultCompression)
	return w
}}

func compressPayload(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := flatePool.Get().(*flate.Writer)
	defer flatePool.Put(w)
	w.Reset(&buf)
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decompressPayload(codec string, data []byte) ([]byte, error) {
	switch codec {
	case codecFlate:
		return io.ReadAll(flate.NewReader(bytes.NewReader(data)))
	default:
		return nil, fmt.Errorf("unknown payload codec %q", codec)
	}
}

// packSpan decides how a span is stored. It returns the attributes/events
// column values and, when the span's heavy parts moved, the compressed payload
// (nil otherwise). min <= 0 disables compression.
func packSpan(attrs, events string, min int) (attrsCol, eventsCol string, blob []byte, err error) {
	if min <= 0 || len(attrs)+len(events) <= min {
		return attrs, events, nil, nil
	}
	lite, attrsChanged := liteAttributes(attrs)
	eventsMoved := len(events) > liteStringBytes
	if !attrsChanged && !eventsMoved {
		return attrs, events, nil, nil // nothing big enough to be worth moving
	}

	var raw bytes.Buffer
	var flags byte
	if attrsChanged {
		flags |= payloadFlagAttrs
	}
	if eventsMoved {
		flags |= payloadFlagEvents
	}
	raw.WriteByte(flags)
	putStr := func(v string) {
		var n [binary.MaxVarintLen64]byte
		raw.Write(n[:binary.PutUvarint(n[:], uint64(len(v)))])
		raw.WriteString(v)
	}
	attrsCol, eventsCol = attrs, events
	if attrsChanged {
		putStr(attrs)
		attrsCol = lite
	}
	if eventsMoved {
		putStr(events)
		eventsCol = eventsSentinel
	}
	blob, err = compressPayload(raw.Bytes())
	return attrsCol, eventsCol, blob, err
}

// liteAttributes replaces string values over liteStringBytes with a ref
// object. changed is false (and lite == attrs) when nothing was replaced or
// the attributes are not a JSON object.
func liteAttributes(attrs string) (lite string, changed bool) {
	dec := json.NewDecoder(strings.NewReader(attrs))
	dec.UseNumber() // keep integers exact
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return attrs, false
	}
	for k, v := range m {
		if str, ok := v.(string); ok && len(str) > liteStringBytes {
			m[k] = map[string]any{payloadRefKey: "payload", "bytes": len(str)}
			changed = true
		}
	}
	if !changed {
		return attrs, false
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return attrs, false
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

// unpackPayload returns the full attributes/events given the stored columns
// and a payload row.
func unpackPayload(codec string, data []byte, attrsCol, eventsCol string) (attrs, events string, err error) {
	raw, err := decompressPayload(codec, data)
	if err != nil {
		return "", "", err
	}
	if len(raw) == 0 {
		return "", "", fmt.Errorf("empty payload")
	}
	flags, rest := raw[0], raw[1:]
	getStr := func() (string, error) {
		n, w := binary.Uvarint(rest)
		if w <= 0 || uint64(len(rest)-w) < n {
			return "", fmt.Errorf("corrupt payload")
		}
		v := string(rest[w : w+int(n)])
		rest = rest[w+int(n):]
		return v, nil
	}
	attrs, events = attrsCol, eventsCol
	if flags&payloadFlagAttrs != 0 {
		if attrs, err = getStr(); err != nil {
			return "", "", err
		}
	}
	if flags&payloadFlagEvents != 0 {
		if events, err = getStr(); err != nil {
			return "", "", err
		}
	}
	return attrs, events, nil
}

// hasPayloadMarker reports whether a stored span may have a payload row. A
// false positive (user data containing the marker) only costs a lookup.
func hasPayloadMarker(attrsCol, eventsCol string) bool {
	return strings.HasPrefix(eventsCol, `[{"$lookout_ref"`) || strings.Contains(attrsCol, `"`+payloadRefKey+`"`)
}

// restorePayloads replaces the lite columns of spans with the full values.
// It issues its own queries, so callers must have closed their row cursors
// (the store has a single connection).
func (s *SQLiteStore) restorePayloads(ctx context.Context, spans []SpanRecord) error {
	var idx []int
	for i := range spans {
		if hasPayloadMarker(spans[i].AttributesJSON, spans[i].EventsJSON) {
			idx = append(idx, i)
		}
	}
	for len(idx) > 0 {
		n := len(idx)
		if n > payloadFetchBatch {
			n = payloadFetchBatch
		}
		batch := idx[:n]
		idx = idx[n:]

		ids := make([]any, len(batch))
		for j, i := range batch {
			ids[j] = spans[i].SpanID
		}
		rows, err := s.db.QueryContext(ctx,
			"SELECT span_id, codec, data FROM span_payloads WHERE span_id IN ("+placeholders(len(ids))+")", ids...)
		if err != nil {
			return err
		}
		type pl struct {
			codec string
			data  []byte
		}
		found := map[string]pl{}
		for rows.Next() {
			var id string
			var p pl
			if err := rows.Scan(&id, &p.codec, &p.data); err != nil {
				rows.Close()
				return err
			}
			found[id] = p
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, i := range batch {
			p, ok := found[spans[i].SpanID]
			if !ok {
				continue
			}
			a, e, err := unpackPayload(p.codec, p.data, spans[i].AttributesJSON, spans[i].EventsJSON)
			if err != nil {
				return fmt.Errorf("span %s payload: %w", spans[i].SpanID, err)
			}
			spans[i].AttributesJSON, spans[i].EventsJSON = a, e
		}
	}
	return nil
}

const (
	compactBatch = 100                   // rowids per transaction
	compactPause = 10 * time.Millisecond // yield between batches so ingest interleaves
)

// CompactStats reports what Compact did.
type CompactStats struct {
	Scanned   int64 // spans over the size threshold that were examined
	Converted int64 // spans moved to compressed payloads
	BytesIn   int64 // attributes + events bytes of converted spans before
	BytesOut  int64 // lite columns + compressed payload bytes after
}

// Compact converts existing large spans (inserted before compression was
// enabled) to the payload layout, in short batched transactions so that
// ingest on a live database keeps interleaving. It is idempotent and
// resumable: converted spans are skipped. min is the size threshold (see
// Options.CompressMinBytes). progress, when non-nil, is called after each batch.
func (s *SQLiteStore) Compact(ctx context.Context, min int, progress func(CompactStats)) (CompactStats, error) {
	var st CompactStats
	if min <= 0 {
		return st, fmt.Errorf("compact needs a positive size threshold")
	}
	var lo, hi sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(rowid), MAX(rowid) FROM spans`).Scan(&lo, &hi); err != nil {
		return st, err
	}
	if !lo.Valid {
		return st, nil
	}
	type row struct {
		rowid          int64
		id, attrs, evs string
	}
	for from := lo.Int64; from <= hi.Int64; from += compactBatch {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT rowid, span_id, attributes_json, events_json FROM spans
			WHERE rowid >= ? AND rowid < ? AND length(attributes_json) + length(events_json) > ?
			  AND NOT EXISTS (SELECT 1 FROM span_payloads p WHERE p.span_id = spans.span_id)`,
			from, from+compactBatch, min)
		if err != nil {
			return st, err
		}
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.rowid, &r.id, &r.attrs, &r.evs); err != nil {
				rows.Close()
				return st, err
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return st, err
		}
		if len(batch) > 0 {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return st, err
			}
			for _, r := range batch {
				st.Scanned++
				a, e, blob, err := packSpan(r.attrs, r.evs, min)
				if err != nil {
					tx.Rollback()
					return st, err
				}
				if blob == nil {
					continue
				}
				// Guarded on the values we read, so a span re-ingested since
				// the SELECT is left alone.
				res, err := tx.ExecContext(ctx,
					`UPDATE spans SET attributes_json = ?, events_json = ? WHERE rowid = ? AND attributes_json = ? AND events_json = ?`,
					a, e, r.rowid, r.attrs, r.evs)
				if err != nil {
					tx.Rollback()
					return st, err
				}
				if n, _ := res.RowsAffected(); n == 0 {
					continue
				}
				if _, err := tx.ExecContext(ctx,
					`INSERT OR REPLACE INTO span_payloads(span_id, codec, data) VALUES (?, ?, ?)`, r.id, codecFlate, blob); err != nil {
					tx.Rollback()
					return st, err
				}
				st.Converted++
				st.BytesIn += int64(len(r.attrs) + len(r.evs))
				st.BytesOut += int64(len(a) + len(e) + len(blob))
			}
			if err := tx.Commit(); err != nil {
				return st, err
			}
			if progress != nil {
				progress(st)
			}
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(compactPause):
		}
	}
	log.Printf("compact: converted %d of %d large spans, %d -> %d bytes", st.Converted, st.Scanned, st.BytesIn, st.BytesOut)
	return st, nil
}

// Vacuum rewrites the database file to return freed pages to the filesystem.
// It holds the write lock for the duration.
func (s *SQLiteStore) Vacuum(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		return err
	}
	s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return nil
}
