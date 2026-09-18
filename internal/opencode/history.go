package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/savid/acp-go-core/process"
)

// ReadHistory reads one conversation graph through the native database command.
// The HTTP history route exports every session in the account in one response.
func ReadHistory(ctx context.Context, executable, scratchDir string, environment []string, id string, cursors map[string]int64) ([]SyncEvent, error) {
	encoded, err := json.Marshal(cursors)
	if err != nil {
		return nil, err
	}

	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	query := `WITH RECURSIVE wanted(id) AS (
 VALUES (` + quote(id) + `)
 UNION SELECT session.id FROM session JOIN wanted ON session.parent_id = wanted.id
), cursors AS (SELECT key, value FROM json_each(` + quote(string(encoded)) + `))
SELECT event.id, event.aggregate_id, event.seq, event.type, event.data
FROM event JOIN wanted ON event.aggregate_id = wanted.id
LEFT JOIN cursors ON cursors.key = event.aggregate_id
WHERE event.seq > COALESCE(cursors.value, -1)
ORDER BY event.seq, event.id`

	output, err := os.CreateTemp(scratchDir, "acp-go-opencode-history-*.json")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = output.Close()
		_ = os.Remove(output.Name())
	}()

	// The native CLI exits before large piped stdout writes finish. A regular
	// file makes its writes synchronous; positional arguments keep the query
	// and executable out of the shell program.
	proc, err := process.Start(ctx, process.Request{
		Executable: "/bin/sh",
		Args:       []string{"-c", `output=$1; shift; exec "$@" > "$output"`, "opencode-history", output.Name(), executable, "db", query, "--format", "json"},
		Env:        environment,
	})
	if err != nil {
		return nil, err
	}

	defer func() {
		select {
		case <-proc.Done():
		default:
			_ = proc.Kill()
		}

		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()

		_, _ = proc.Wait(closeCtx)
		_ = proc.Close()
	}()

	stop := context.AfterFunc(ctx, func() { _ = proc.Kill() })
	defer stop()

	_ = proc.Stdin().Close()

	result, err := proc.Wait(ctx)
	if err != nil {
		return nil, err
	}

	if result.ExitCode != 0 {
		return nil, fmt.Errorf("opencode database query exited with status %d", result.ExitCode)
	}

	data, err := io.ReadAll(io.LimitReader(output, MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}

	if len(data) > MaxBodyBytes {
		return nil, errors.New("native session history exceeds size limit")
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}

	events := make([]SyncEvent, 0, len(rows))
	for _, row := range rows {
		var payload string
		if err := json.Unmarshal(row["data"], &payload); err != nil {
			return nil, err
		}

		row["data"] = json.RawMessage(payload)

		raw, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}

		var event SyncEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, err
		}

		events = append(events, event)
	}

	return events, nil
}
