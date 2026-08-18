package opencodeacp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// wireTransport drives one agent connection over the raw newline-delimited
// transport and hands back every line the agent wrote, in the order it wrote
// them. Order on the transport is the only place the post-response rule can be
// observed: a decoded notification says nothing about which line left first.
type wireTransport struct {
	requests io.WriteCloser
	written  chan map[string]any
}

func newWireTransport(t *testing.T, agent *Agent) *wireTransport {
	t.Helper()

	requestReader, requestWriter := io.Pipe()
	responseReader, responseWriter := io.Pipe()

	connection := newLocalAgentConnection(agent, responseWriter, requestReader)
	agent.setAgentClient(connection)

	transport := &wireTransport{requests: requestWriter, written: make(chan map[string]any, 32)}

	go func() {
		scanner := bufio.NewScanner(responseReader)
		for scanner.Scan() {
			var frame map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
				continue
			}

			transport.written <- frame
		}
	}()

	t.Cleanup(func() {
		_ = requestWriter.Close()
		_ = requestReader.Close()
		_ = responseWriter.Close()
		_ = responseReader.Close()
	})

	return transport
}

func (w *wireTransport) send(t *testing.T, id int, method string, params map[string]any) {
	t.Helper()

	line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	require.NoError(t, err)

	_, err = w.requests.Write(append(line, '\n'))
	require.NoError(t, err)
}

// next reads the next line the agent wrote.
func (w *wireTransport) next(t *testing.T) map[string]any {
	t.Helper()

	select {
	case frame := <-w.written:
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("the agent wrote no further line")

		return nil
	}
}

// TestEstablishmentIsWrittenAfterTheEstablishingResponse proves the rule on the
// only surface that can carry it: the establishing response leaves the transport
// first, and the opening lifecycle snapshot and the initial command catalog
// follow it, in that order.
func TestEstablishmentIsWrittenAfterTheEstablishingResponse(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native")
	client.forkSession = testNativeSession("native-child")
	client.getSession = testNativeSession("native-child")
	client.ensureSyncAggregate("native-child")
	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review changes"}}

	agent := NewAgent()
	agent.runtime = client

	t.Cleanup(func() { _ = agent.Close() })

	transport := newWireTransport(t, agent)
	transport.send(t, 1, acp.AgentMethodInitialize, map[string]any{
		"protocolVersion":    acp.ProtocolVersionNumber,
		"clientCapabilities": map[string]any{},
		"_meta":              lifecycleOffer(),
	})
	require.EqualValues(t, 1, transport.next(t)["id"])

	transport.send(t, 2, acp.AgentMethodSessionNew, map[string]any{
		jsonFieldCwd: t.TempDir(),
		"mcpServers": []any{},
	})

	response := transport.next(t)
	require.EqualValues(t, 2, response["id"], "the establishing response is not the first line written")

	result, ok := response["result"].(map[string]any)
	require.True(t, ok)
	require.NotEmpty(t, result["sessionId"])

	snapshot := transport.next(t)
	require.Equal(t, acp.ClientMethodSessionUpdate, snapshot["method"])
	require.Equal(t, "lifecycle_snapshot", wireLifecycleEventType(t, snapshot))

	catalog := transport.next(t)
	require.Equal(t, acp.ClientMethodSessionUpdate, catalog["method"])

	update, ok := catalog["params"].(map[string]any)["update"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, sessionUpdateAvailableCommands, update["sessionUpdate"])

	// The fork route establishes a session too, and it reaches the transport
	// through the extension dispatcher rather than the stable one.
	transport.send(t, 3, ForkSessionMethod, map[string]any{
		"sessionId":  result["sessionId"],
		jsonFieldCwd: t.TempDir(),
	})

	forked := transport.next(t)
	require.EqualValues(t, 3, forked["id"], "the fork response is not the first line written")

	forkSnapshot := transport.next(t)
	require.Equal(t, "lifecycle_snapshot", wireLifecycleEventType(t, forkSnapshot))

	forkCatalog := transport.next(t)
	forkUpdate, ok := forkCatalog["params"].(map[string]any)["update"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, sessionUpdateAvailableCommands, forkUpdate["sessionUpdate"])
}

// wireLifecycleEventType reads the event type off a notification's lifecycle
// envelope.
func wireLifecycleEventType(t *testing.T, frame map[string]any) string {
	t.Helper()

	params, ok := frame["params"].(map[string]any)
	require.True(t, ok)

	meta, ok := params["_meta"].(map[string]any)
	require.True(t, ok, "the notification carries no _meta")

	envelope, ok := meta[lifecycle.MetaKey].(map[string]any)
	require.True(t, ok, "the notification carries no lifecycle envelope")

	event, ok := envelope["event"].(map[string]any)
	require.True(t, ok)

	eventType, ok := event["type"].(string)
	require.True(t, ok)

	return eventType
}

// TestEstablishmentHookIgnoresWhatItCannotEstablish proves the queue releases
// nothing for a frame that established no session: a method that opens none, a
// request that never reached the transport tagger, and a response the agent
// answered with an error each leave the hook unrun.
func TestEstablishmentHookIgnoresWhatItCannotEstablish(t *testing.T) {
	agent := NewAgent()
	hooks := newEstablishmentHooks(agent.log)
	connection := &localAgentConnection{agent: agent, hooks: hooks}

	tagged, err := json.Marshal(map[string]any{
		"sessionId":            "session-1",
		establishmentHookParam: "7",
		jsonFieldCwd:           "/repo",
	})
	require.NoError(t, err)

	// A method that establishes nothing, and an establishing method whose
	// request never carried a response id.
	connection.queueEstablishment(context.Background(), acp.AgentMethodSessionList, tagged, acp.ListSessionsResponse{})
	connection.queueEstablishment(context.Background(), acp.AgentMethodSessionLoad,
		json.RawMessage(`{"sessionId":"session-1"}`), acp.LoadSessionResponse{})
	require.Empty(t, hooks.all)

	// A queued hook whose response was written as an error is dropped rather
	// than run: nothing was established for it to speak for.
	connection.queueEstablishment(context.Background(), acp.AgentMethodSessionLoad, tagged, acp.LoadSessionResponse{})
	require.Len(t, hooks.all, 1)
	hooks.runAfterWrite([]byte(`{"jsonrpc":"2.0","id":7,"error":{"code":-32603}}`))
	require.Empty(t, hooks.all)

	// A frame that is not a response releases nothing: an unreadable line, a
	// notification with no id, and a response no queued hook is waiting for.
	connection.queueEstablishment(context.Background(), acp.AgentMethodSessionLoad, tagged, acp.LoadSessionResponse{})
	hooks.runAfterWrite([]byte(`{not json`))
	hooks.runAfterWrite([]byte(`{"jsonrpc":"2.0","method":"session/update"}`))
	hooks.runAfterWrite([]byte(`{"jsonrpc":"2.0","id":9,"result":{}}`))
	require.Len(t, hooks.all, 1)
}

// TestEstablishedSessionIDReadsEachRouteWhereItCarriesIt proves the four routes
// are read where each one names its session, and that a route naming none —
// or a request body that cannot be read — establishes nothing.
func TestEstablishedSessionIDReadsEachRouteWhereItCarriesIt(t *testing.T) {
	t.Parallel()

	created, ok := establishedSessionID(acp.AgentMethodSessionNew, nil, acp.NewSessionResponse{SessionId: "session-new"})
	require.True(t, ok)
	require.EqualValues(t, "session-new", created)

	_, ok = establishedSessionID(acp.AgentMethodSessionNew, nil, acp.ListSessionsResponse{})
	require.False(t, ok, "a response of the wrong type named a session")

	loaded, ok := establishedSessionID(acp.AgentMethodSessionLoad,
		json.RawMessage(`{"sessionId":"session-load"}`), acp.LoadSessionResponse{})
	require.True(t, ok)
	require.EqualValues(t, "session-load", loaded)

	_, ok = establishedSessionID(acp.AgentMethodSessionResume, json.RawMessage(`{bad`), acp.ResumeSessionResponse{})
	require.False(t, ok, "an unreadable request named a session")

	forked, ok := establishedSessionID(ForkSessionMethod, nil, acp.UnstableForkSessionResponse{SessionId: "session-fork"})
	require.True(t, ok)
	require.EqualValues(t, "session-fork", forked)

	require.Empty(t, establishmentHookID(json.RawMessage(`{bad`)))
}

// TestEstablishmentTagReaderTagsOnlyEstablishingRequests proves the transport
// tagger stamps the response id on the four establishing requests and passes
// every other line through byte for byte, including the last line of a stream
// that ends without a newline.
func TestEstablishmentTagReaderTagsOnlyEstablishingRequests(t *testing.T) {
	t.Parallel()

	untouched := []string{
		`not json at all`,
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"session-1"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"session-1"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/new"}`,
		`{"jsonrpc":"2.0","id":3,"method":"session/new","params":[]}`,
	}

	for _, line := range untouched {
		require.Equal(t, line, string(tagEstablishingRequest([]byte(line))), line)
	}

	tagged := tagEstablishingRequest([]byte(`{"jsonrpc":"2.0","id":4,"method":"session/new","params":{"cwd":"/repo"}}` + "\n"))
	require.Equal(t, byte('\n'), tagged[len(tagged)-1])

	var frame struct {
		Params json.RawMessage `json:"params"`
	}

	require.NoError(t, json.Unmarshal(tagged, &frame))
	require.Equal(t, "4", establishmentHookID(frame.Params))

	// The reader hands the tagged line on in fragments and reports the stream's
	// own end only once it has delivered the last line it read.
	reader := newEstablishmentTagReader(strings.NewReader(
		`{"jsonrpc":"2.0","id":5,"method":"session/load","params":{"sessionId":"session-1"}}`))

	var read []byte

	for {
		buffer := make([]byte, 16)

		count, err := reader.Read(buffer)
		read = append(read, buffer[:count]...)

		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}
	}

	require.NoError(t, json.Unmarshal(read, &frame))
	require.Equal(t, "5", establishmentHookID(frame.Params))

	count, err := reader.Read(make([]byte, 16))
	require.Zero(t, count)
	require.ErrorIs(t, err, io.EOF)
}

// TestEstablishmentWriterReleasesNothingOnAFailedWrite proves the hook waits on
// a response that actually left this process: a short or failed write delivered
// no response for it to follow.
func TestEstablishmentWriterReleasesNothingOnAFailedWrite(t *testing.T) {
	t.Parallel()

	hooks := newEstablishmentHooks(slog.New(slog.DiscardHandler))
	ran := make(chan struct{})
	hooks.queue("11", func() { close(ran) })

	writer := hooks.wrap(shortWriter{})
	count, err := writer.Write([]byte(`{"jsonrpc":"2.0","id":11,"result":{}}`))
	require.Zero(t, count)
	require.NoError(t, err)

	failing := hooks.wrap(failingWriter{err: errors.New("transport gone")})
	_, err = failing.Write([]byte(`{"jsonrpc":"2.0","id":11,"result":{}}`))
	require.ErrorContains(t, err, "transport gone")

	select {
	case <-ran:
		t.Fatal("a hook ran for a response that never reached the transport")
	default:
	}

	require.Len(t, hooks.all, 1)
}

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) { return 0, nil }

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// TestEstablishmentReportsEveryStageItCannotComplete proves each stage of the
// post-response work is diagnostic on its own: a session that is already gone, a
// stream that cannot be opened, and a catalog that cannot be fetched are each
// reported without disturbing the others.
func TestEstablishmentReportsEveryStageItCannotComplete(t *testing.T) {
	agent := negotiatedAgent(t)
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	client := newFakeOpenCodeClient()
	client.commandsErr = errors.New("commands failed")
	current := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	local := &localAgentConnection{agent: agent, hooks: newEstablishmentHooks(agent.log)}

	// A session the agent no longer holds is reported and nothing else runs.
	local.establishSession(context.Background(), acp.AgentMethodSessionLoad, "missing")

	// An established session whose catalog fetch fails still keeps its stream.
	local.establishSession(context.Background(), acp.AgentMethodSessionNew, current.id)
	require.Equal(t, []string{"lifecycle_snapshot"}, connection.lifecycleEvents(t))
	require.Empty(t, connection.availableCommandUpdates())

	// A stream that cannot be delivered is reported on the same terms.
	broken := lifecycleSessionWithBrokenStream(t)
	local = &localAgentConnection{agent: broken.agent, hooks: newEstablishmentHooks(broken.agent.log)}
	local.establishSession(context.Background(), acp.AgentMethodSessionResume, broken.id)
	require.ErrorContains(t, broken.lifecycleFailure(), "wire down")
}

// TestPromptEstablishesTheSessionItCannotAssumeWasEstablished proves the prompt
// path opens the stream itself for a host driving this Agent in process, and
// refuses the turn when that stream cannot be opened rather than accepting a
// submission on a stream nothing opened.
func TestPromptEstablishesTheSessionItCannotAssumeWasEstablished(t *testing.T) {
	current := lifecycleSessionWithBrokenStream(t)

	_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
	require.ErrorContains(t, err, "wire down")
}

// lifecycleSessionWithBrokenStream builds a negotiated session whose host cannot
// receive a notification.
func lifecycleSessionWithBrokenStream(t *testing.T) *session {
	t.Helper()

	agent := negotiatedAgent(t)
	connection := newRecordingAgentClient()
	connection.updateErr = errors.New("wire down")
	agent.setAgentClient(connection)

	client := newFakeOpenCodeClient()
	current := newSession(agent, "session-broken", "/tmp/project", nil, testNativeSession("native-broken"), client, sessionMeta{}, idmapRecord{
		SessionID: "session-broken", NativeSessionID: "native-broken", Format: SessionStoreFormat,
	})

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	t.Cleanup(current.stopPump)

	return current
}
