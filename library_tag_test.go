package scaffold_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/kapetan-io/scaffold.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a goroutine-safe sink for JSON log output. Scaffold emits
// lifecycle lines and binding serve-loop lines from separate goroutines, so
// the capture target must serialize concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// parseLogLines decodes each newline-delimited JSON record emitted by a
// slog.JSONHandler into a map keyed by attribute name.
func parseLogLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		records = append(records, rec)
	}
	return records
}

// findLine returns the first record whose "msg" equals msg, or nil.
func findLine(records []map[string]any, msg string) map[string]any {
	for _, rec := range records {
		if rec["msg"] == msg {
			return rec
		}
	}
	return nil
}

// rawLine returns the raw JSON text of the first line whose "msg" equals msg,
// or "". Used to detect duplicate attribute keys, which json.Unmarshal into a
// map would silently collapse.
func rawLine(out, msg string) string {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, `"msg":"`+msg+`"`) {
			return line
		}
	}
	return ""
}

// Scaffold's own lifecycle log lines carry library=scaffold, while service
// code logging through the logger scaffold hands it (sc.Log) carries no
// library field.
func TestLifecycleLogLinesCarryLibraryScaffold(t *testing.T) {
	buf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	d := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Log.Info("service started")
			return nil
		},
	}

	inst, err := scaffold.Start(context.Background(), asDaemon(d), &scaffold.Options{Log: log})
	require.NoError(t, err)
	require.NoError(t, inst.Stop(context.Background()))

	records := parseLogLines(t, buf.String())

	for _, msg := range []string{"daemon starting", "daemon ready", "shutdown initiated", "daemon stopped"} {
		rec := findLine(records, msg)
		require.NotNil(t, rec)
		assert.Equal(t, "scaffold", rec["library"])
	}

	svc := findLine(records, "service started")
	require.NotNil(t, svc)
	_, tagged := svc["library"]
	assert.False(t, tagged)
}

// A downstream library that self-tags the logger scaffold handed it (e.g.
// leader's .With("library","leader")) produces a single library=leader, with
// no duplicate key inherited from scaffold.
func TestServiceLibraryTagHasNoDuplicate(t *testing.T) {
	buf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	d := &fakeDaemon{
		OnStart: func(_ context.Context, sc *scaffold.DaemonConfig) error {
			sc.Log.With("library", "leader").Info("leader elected")
			return nil
		},
	}

	inst, err := scaffold.Start(context.Background(), asDaemon(d), &scaffold.Options{Log: log})
	require.NoError(t, err)
	require.NoError(t, inst.Stop(context.Background()))

	out := buf.String()
	leader := findLine(parseLogLines(t, out), "leader elected")
	require.NotNil(t, leader)
	assert.Equal(t, "leader", leader["library"])

	assert.Equal(t, 1, strings.Count(rawLine(out, "leader elected"), `"library"`))
}
