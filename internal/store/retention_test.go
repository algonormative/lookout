package store

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func optStore(t *testing.T, opts Options) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "test.db"), opts)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// bigSpan is a ~100 KB span whose trace starts at the given time.
func bigSpan(trace string, start time.Time) SpanRecord {
	return SpanRecord{
		TraceID: trace, SpanID: trace + "-s", Name: "n",
		AttributesJSON:    `{"k":"` + strings.Repeat("x", 100_000) + `"}`,
		StartTimeUnixNano: uint64(start.UnixNano()), EndTimeUnixNano: uint64(start.UnixNano()) + 1,
		InsertedAt: time.Now(),
	}
}

func TestCapDeletesOldestTracesFirstAndKeepsScores(t *testing.T) {
	s := optStore(t, Options{MaxDBBytes: 1 << 20})
	ctx := context.Background()

	base := time.Now().Add(-time.Hour)
	var spans []SpanRecord
	for i := 0; i < 30; i++ {
		spans = append(spans, bigSpan(fmt.Sprintf("t%02d", i), base.Add(time.Duration(i)*time.Minute)))
	}
	// A second span of the oldest trace that starts late: the whole trace must
	// go together even though this span is newer than other traces.
	late := bigSpan("t00", base.Add(40*time.Minute))
	late.SpanID = "t00-late"
	spans = append(spans, late)
	if err := s.InsertSpans(ctx, spans); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertScores(ctx, []Score{{TraceID: "t00", Name: "q", DataType: ScoreBoolean, Source: "x",
		ValueNum: ptr(1.0)}}); err != nil {
		t.Fatal(err)
	}

	if used, _ := s.usedBytes(ctx); used < 3<<20 {
		t.Fatalf("test setup: used %d, want > 3MB", used)
	}
	n, err := s.Cleanup(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("cap deleted nothing")
	}
	used, _ := s.usedBytes(ctx)
	if used > (1<<20)/100*90 {
		t.Errorf("used %d after cap, want <= 90%% of cap", used)
	}

	left, _ := s.QuerySpans(ctx, SpanFilter{Limit: 100, SortBy: "time"})
	have := map[string]bool{}
	for _, sp := range left {
		have[sp.TraceID] = true
	}
	if have["t00"] || have["t01"] {
		t.Errorf("oldest traces should be gone: %v", have)
	}
	if !have["t29"] || !have["t28"] {
		t.Errorf("newest traces must survive: %v", have)
	}
	// Whole traces only: t00's late span is gone with it.
	if tr, _ := s.GetTrace(ctx, "t00"); len(tr) != 0 {
		t.Errorf("trace t00 partially kept: %d spans", len(tr))
	}
	// Oldest-first means survivors are a suffix: no gaps.
	seenKept := false
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("t%02d", i)
		if have[id] {
			seenKept = true
		} else if seenKept {
			t.Errorf("gap: %s missing but an older trace survived", id)
		}
	}
	sc, _, err := s.QueryScores(ctx, ScoreFilter{TraceID: "t00", Limit: 10})
	if err != nil || len(sc) != 1 {
		t.Errorf("score for deleted trace must survive: %v %v", sc, err)
	}
	// The cap forces a vacuum, so the file really shrank.
	var free int
	s.db.QueryRow("PRAGMA freelist_count").Scan(&free)
	if free != 0 {
		t.Errorf("freelist=%d after cap vacuum", free)
	}
}

func TestCapDisabledAndUnderCapDoNothing(t *testing.T) {
	for _, max := range []int64{0, 1 << 30} {
		s := optStore(t, Options{MaxDBBytes: max})
		ctx := context.Background()
		s.InsertSpans(ctx, []SpanRecord{bigSpan("a", time.Now()), bigSpan("b", time.Now())})
		if n, err := s.Cleanup(ctx, 7); err != nil || n != 0 {
			t.Errorf("max=%d: n=%d err=%v", max, n, err)
		}
	}
}

func readArchive(t *testing.T, path string) []SpanRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f) // multistream: every appended member
	if err != nil {
		t.Fatal(err)
	}
	var out []SpanRecord
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var sp SpanRecord
		if err := json.Unmarshal(sc.Bytes(), &sp); err != nil {
			t.Fatalf("bad archive line: %v", err)
		}
		out = append(out, sp)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestArchiveRoundTripRetention(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "arch")
	s := optStore(t, Options{ArchiveDir: dir})
	ctx := context.Background()

	d1 := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(24 * time.Hour).Add(3 * time.Hour)
	d2 := d1.Add(24 * time.Hour)
	mk := func(id string, at time.Time) SpanRecord {
		sp := bigSpan(id, at)
		sp.EventsJSON = `[{"name":"e","attributes":{"a":"é <b>"}}]`
		sp.Name = "name-" + id
		return sp
	}
	in := []SpanRecord{mk("a", d1), mk("b", d1.Add(time.Minute)), mk("c", d2), bigSpan("fresh", time.Now())}
	s.InsertSpans(ctx, in)

	n, err := s.Cleanup(ctx, 7)
	if err != nil || n != 3 {
		t.Fatalf("cleanup n=%d err=%v", n, err)
	}
	got1 := readArchive(t, filepath.Join(dir, "spans-"+d1.Format("2006-01-02")+".jsonl.gz"))
	got2 := readArchive(t, filepath.Join(dir, "spans-"+d2.Format("2006-01-02")+".jsonl.gz"))
	if len(got1) != 2 || len(got2) != 1 {
		t.Fatalf("archive sizes %d, %d", len(got1), len(got2))
	}
	byID := map[string]SpanRecord{}
	for _, sp := range append(got1, got2...) {
		byID[sp.TraceID] = sp
	}
	for _, want := range in[:3] {
		g := byID[want.TraceID]
		if g.AttributesJSON != want.AttributesJSON || g.EventsJSON != want.EventsJSON || g.Name != want.Name ||
			g.StartTimeUnixNano != want.StartTimeUnixNano {
			t.Errorf("round trip mismatch for %s", want.TraceID)
		}
	}
	if left, _ := s.QuerySpans(ctx, SpanFilter{Limit: 10}); len(left) != 1 || left[0].TraceID != "fresh" {
		t.Errorf("fresh span should remain, got %v", left)
	}

	// A later cleanup appends a new gzip member to the same day file.
	s.InsertSpans(ctx, []SpanRecord{mk("d", d1.Add(2*time.Minute))})
	if n, err := s.Cleanup(ctx, 7); err != nil || n != 1 {
		t.Fatalf("second cleanup n=%d err=%v", n, err)
	}
	if got := readArchive(t, filepath.Join(dir, "spans-"+d1.Format("2006-01-02")+".jsonl.gz")); len(got) != 3 {
		t.Errorf("append produced %d lines, want 3", len(got))
	}
}

func TestArchiveRoundTripCap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "arch")
	s := optStore(t, Options{ArchiveDir: dir, MaxDBBytes: 1 << 20})
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 20; i++ {
		s.InsertSpans(ctx, []SpanRecord{bigSpan(fmt.Sprintf("t%02d", i), base.Add(time.Duration(i)*time.Second))})
	}
	n, err := s.Cleanup(ctx, 7)
	if err != nil || n == 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "spans-*.jsonl.gz"))
	archived := 0
	for _, f := range files {
		archived += len(readArchive(t, f))
	}
	if int64(archived) != n {
		t.Errorf("archived %d spans, deleted %d", archived, n)
	}
}

func TestFailedArchiveWritePreventsDeletion(t *testing.T) {
	// ArchiveDir is a regular file, so MkdirAll / open fails.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := optStore(t, Options{ArchiveDir: blocker, MaxDBBytes: 1 << 20})
	ctx := context.Background()
	old := time.Now().Add(-30 * 24 * time.Hour)
	spans := []SpanRecord{bigSpan("old", old)}
	for i := 0; i < 20; i++ {
		spans = append(spans, bigSpan(fmt.Sprintf("t%02d", i), time.Now().Add(-time.Duration(i)*time.Minute)))
	}
	s.InsertSpans(ctx, spans)

	n, err := s.Cleanup(ctx, 7)
	if err == nil {
		t.Fatal("expected archive error")
	}
	if n != 0 {
		t.Errorf("deleted %d spans despite failed archive", n)
	}
	all, _ := s.QuerySpans(ctx, SpanFilter{Limit: 100})
	if len(all) != 21 {
		t.Errorf("have %d spans, want all 21", len(all))
	}
}

func ptr[T any](v T) *T { return &v }
