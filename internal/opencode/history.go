package opencode

import (
	"bufio"
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

// SyncEvent is one native sync event as the mirror stores it: Data is the
// event's JSON object. Its encoding is the mirror row: members in name order,
// data compact.
//
//nolint:tagliatelle // The native event table names its aggregate column aggregate_id.
type SyncEvent struct {
	AggregateID string          `json:"aggregate_id"`
	Data        json.RawMessage `json:"data"`
	ID          string          `json:"id"`
	Sequence    int64           `json:"seq"`
	Type        string          `json:"type"`
}

// ReadHistory reads the sync events of one conversation graph through the
// native database command: the session id and every descendant, each from
// after its cursor, or whole when the cursor map has no entry for it. It
// returns each event as its encoded mirror row, ordered by sequence and id.
// The HTTP history route exports every session in the account in one response.
func ReadHistory(ctx context.Context, executable, scratchDir string, environment []string, id string, cursors map[string]int64) ([][]byte, error) {
	if cursors == nil {
		cursors = map[string]int64{}
	}

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

	// The native CLI exits before large piped stdout writes finish, so its
	// output goes straight to the file.
	proc, err := process.Start(ctx, process.Request{
		Executable: executable,
		Args:       []string{"db", query, "--format", "json"},
		Env:        environment,
		Stdout:     output,
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

	info, err := output.Stat()
	if err != nil {
		return nil, err
	}

	if info.Size() > MaxBodyBytes {
		return nil, errors.New("native session history exceeds size limit")
	}

	// The child wrote through a duplicate of this descriptor, so the shared
	// offset sits at the end of its output.
	if _, err = output.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	return DecodeHistory(bufio.NewReader(output))
}

// DecodeHistory decodes the native database command's JSON rows into mirror
// rows one row at a time. The database keeps each event's data as JSON text.
//
//nolint:tagliatelle // The native event table names its aggregate column aggregate_id.
func DecodeHistory(reader io.Reader) ([][]byte, error) {
	decoder := json.NewDecoder(reader)

	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return nil, errors.New("native history is not a row array")
	}

	var rows [][]byte

	for decoder.More() {
		var row struct {
			ID          string `json:"id"`
			AggregateID string `json:"aggregate_id"`
			Sequence    int64  `json:"seq"`
			Type        string `json:"type"`
			Data        string `json:"data"`
		}
		if err := decoder.Decode(&row); err != nil {
			return nil, err
		}

		data := strings.TrimSpace(row.Data)
		if !strings.HasPrefix(data, "{") {
			return nil, errors.New("native sync event data is not an object")
		}

		encoded, err := json.Marshal(SyncEvent{ID: row.ID, AggregateID: row.AggregateID, Sequence: row.Sequence, Type: row.Type, Data: json.RawMessage(data)})
		if err != nil {
			return nil, err
		}

		rows = append(rows, encoded)
	}

	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return nil, errors.New("native history row array is incomplete")
	}

	return rows, nil
}
