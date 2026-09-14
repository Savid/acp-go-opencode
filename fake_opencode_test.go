package opencodeacp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

const fakeOpenCodeEnv = "ACP_GO_OPENCODE_TEST_FAKE"
const fakeOpenCodeEnvVersion = "ACP_GO_OPENCODE_TEST_VERSION"

type fakeOpenCode struct {
	mu          sync.Mutex
	rows        []opencode.SyncEvent
	sessions    map[string]opencode.NativeSession
	subscribers map[chan []byte]bool
	pending     map[string]chan struct{}
	answers     map[string]chan json.RawMessage
	path        string
}

func runFakeOpenCode(args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		version := os.Getenv(fakeOpenCodeEnvVersion)
		if version == "" {
			version = "1.18.30"
		}
		fmt.Println(version)

		return 0
	}
	port := ""
	for i, arg := range args {
		if arg == "--port" && i+1 < len(args) {
			port = args[i+1]
		}
	}
	if port == "" {
		return 2
	}
	f := &fakeOpenCode{sessions: map[string]opencode.NativeSession{}, subscribers: map[chan []byte]bool{}, pending: map[string]chan struct{}{}, answers: map[string]chan json.RawMessage{}, path: filepath.Join(os.Getenv("XDG_DATA_HOME"), "opencode", "fake.json")}
	if data, err := os.ReadFile(f.path); err == nil {
		if json.Unmarshal(data, &f.rows) != nil {
			return 3
		}
		for _, event := range f.rows {
			if strings.HasPrefix(event.Type, "session.") {
				var s opencode.NativeSession
				if json.Unmarshal(event.Data["info"], &s) == nil && s.ID != "" {
					f.sessions[s.ID] = s
				}
			}
		}
	}
	server := &http.Server{Addr: "127.0.0.1:" + port, Handler: f, ReadHeaderTimeout: time.Second}
	if err := server.ListenAndServe(); err != nil {
		return 4
	}

	return 0
}
func (f *fakeOpenCode) publish(typ string, properties any) {
	data, _ := json.Marshal(map[string]any{"directory": "", "payload": map[string]any{"id": opencode.NewID("evt_"), "type": typ, "properties": properties}})
	for ch := range f.subscribers {
		select {
		case ch <- data:
		default:
			panic("fake SSE overflow")
		}
	}
}
func (f *fakeOpenCode) append(id, typ string, data map[string]any) {
	seq := int64(0)
	for _, event := range f.rows {
		if event.AggregateID == id {
			seq = event.Sequence + 1
		}
	}
	encoded := map[string]json.RawMessage{}
	data["sessionID"] = id
	for key, value := range data {
		encoded[key], _ = json.Marshal(value)
	}
	f.rows = append(f.rows, opencode.SyncEvent{ID: opencode.NewID("evt_"), AggregateID: id, Sequence: seq, Type: typ + ".1", Data: encoded})
	f.save()
	f.publish(typ, data)
}
func (f *fakeOpenCode) save() {
	data, _ := json.Marshal(f.rows)
	_ = os.MkdirAll(filepath.Dir(f.path), 0700)
	if err := os.WriteFile(f.path, data, 0600); err != nil {
		panic(err)
	}
}
func (f *fakeOpenCode) messages(id string) []opencode.NativeMessage {
	rows := make([][]byte, 0, len(f.rows))
	for _, event := range f.rows {
		row, _ := json.Marshal(event)
		rows = append(rows, row)
	}

	return nativeMessages(rows, id)
}
func fakeWrite(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func (f *fakeOpenCode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	username, password, ok := r.BasicAuth()
	if !ok || username != "opencode" || password != os.Getenv("OPENCODE_SERVER_PASSWORD") {
		w.WriteHeader(http.StatusUnauthorized)

		return
	}
	if r.URL.Path == "/global/event" {
		f.events(w, r)

		return
	}
	if strings.HasSuffix(r.URL.Path, "/message") && r.Method == http.MethodPost || strings.HasSuffix(r.URL.Path, "/command") && r.Method == http.MethodPost {
		f.prompt(w, r)

		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch path {
	case "/global/health":
		fakeWrite(w, map[string]any{"healthy": true, "version": "1.18.30"})

		return
	case "/doc":
		paths := map[string]any{}
		for _, p := range []string{"/sync/history", "/sync/replay", "/command", "/session/{sessionID}/message", "/session/{sessionID}/command", "/permission/{requestID}/reply", "/question/{requestID}/reply"} {
			paths[p] = map[string]any{}
		}
		fakeWrite(w, map[string]any{"paths": paths, "components": map[string]any{"schemas": map[string]any{"OutputFormatJsonSchema": map[string]any{}}}})

		return
	case "/config/providers":
		fakeWrite(w, map[string]any{"providers": []any{map[string]any{"id": "fake", "name": "Fake", "models": map[string]any{"vision": map[string]any{"id": "vision", "name": "Vision", "limit": map[string]any{"context": 32000}, "capabilities": map[string]any{"input": map[string]any{"image": true}}, "variants": map[string]any{"low": map[string]any{}, "high": map[string]any{}}}, "text": map[string]any{"id": "text", "name": "Text", "capabilities": map[string]any{"input": map[string]any{"image": false}}}}}}, "default": map[string]string{"fake": "vision"}})

		return
	case "/agent":
		f.agents(w, r)

		return
	case "/command":
		fakeWrite(w, []map[string]string{{"name": "inspect", "description": "Inspect workspace"}})

		return
	case "/session/status":
		statuses := map[string]any{}
		for id := range f.pending {
			statuses[id] = map[string]string{"type": "busy"}
		}
		fakeWrite(w, statuses)

		return
	case "/sync/history":
		var cursors map[string]int64
		_ = json.NewDecoder(r.Body).Decode(&cursors)
		events := []opencode.SyncEvent{}
		for _, event := range f.rows {
			if n, ok := cursors[event.AggregateID]; !ok || event.Sequence > n {
				events = append(events, event)
			}
		}
		fakeWrite(w, events)

		return
	case "/sync/replay":
		f.replay(w, r)

		return
	case "/session":
		var session opencode.NativeSession
		_ = json.NewDecoder(r.Body).Decode(&session)
		session.ID = opencode.NewID("ses_")
		session.Directory = r.URL.Query().Get("directory")
		session.Time.Created = time.Now().UnixMilli()
		session.Time.Updated = session.Time.Created
		if session.Model.ID == "" {
			session.Model.ID = "vision"
			session.Model.ProviderID = "fake"
		}
		if session.Agent == "" {
			session.Agent = "build"
		}
		f.sessions[session.ID] = session
		f.append(session.ID, "session.created", map[string]any{"info": session})
		fakeWrite(w, session)

		return
	}
	f.sessionRoute(w, r, path)
}
func (f *fakeOpenCode) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	ch := make(chan []byte, 256)
	f.mu.Lock()
	f.subscribers[ch] = true
	f.mu.Unlock()
	defer func() { f.mu.Lock(); delete(f.subscribers, ch); f.mu.Unlock() }()
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	for {
		select {
		case data := <-ch:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
func (f *fakeOpenCode) prompt(w http.ResponseWriter, r *http.Request) {
	id := strings.Split(strings.Trim(r.URL.Path, "/"), "/")[1]
	var request opencode.MessageRequest
	if json.NewDecoder(r.Body).Decode(&request) != nil {
		w.WriteHeader(400)

		return
	}
	f.mu.Lock()
	session, exists := f.sessions[id]
	if !exists {
		f.mu.Unlock()
		w.WriteHeader(404)

		return
	}
	text := ""
	for _, part := range request.Parts {
		if v, ok := part["text"].(string); ok {
			text += v
		}
	}
	if text == "CRASH" {
		os.Exit(17)
	}
	if text == "REFUSE" {
		f.mu.Unlock()
		w.WriteHeader(400)
		fakeWrite(w, map[string]any{"name": "APIError", "data": map[string]any{"message": "provider refused prompt"}})

		return
	}
	user := opencode.NativeMessageInfo{ID: request.MessageID, SessionID: id, Role: roleUser}
	f.append(id, "message.updated", map[string]any{"info": user})
	f.append(id, "message.part.updated", map[string]any{"part": opencode.NativePart{ID: opencode.NewID("prt_"), MessageID: user.ID, SessionID: id, Type: "text", Text: text}, "time": time.Now().UnixMilli()})
	info := opencode.NativeMessageInfo{ID: opencode.NewMessageID(), ParentID: user.ID, SessionID: id, Role: roleAssistant, ModelID: session.Model.ID, ProviderID: session.Model.ProviderID}
	if request.Model != nil {
		info.ModelID = request.Model.ModelID
		info.ProviderID = request.Model.ProviderID
	}
	f.append(id, "message.updated", map[string]any{"info": info})
	pending := make(chan struct{})
	f.pending[id] = pending
	f.mu.Unlock()
	if text == "SLOW" {
		select {
		case <-pending:
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pending, id)
	select {
	case <-pending:
		info.Finish = "interrupted"
	default:
		info.Finish = "stop"
	}
	if text == "PERMISSION" || text == "QUESTION" {
		callID := opencode.NewID("call_")
		part := opencode.NativePart{ID: opencode.NewID("prt_"), SessionID: id, MessageID: info.ID, Type: "tool", CallID: callID, Tool: "bash", State: json.RawMessage(`{"status":"running","input":{"command":"echo test"}}`)}
		f.append(id, "message.part.updated", map[string]any{"part": part, "time": time.Now().UnixMilli()})
		requestID := opencode.NewID("req_")
		answer := make(chan json.RawMessage, 1)
		f.answers[requestID] = answer
		if text == "PERMISSION" {
			f.publish("permission.asked", opencode.PermissionRequest{ID: requestID, SessionID: id, Permission: "bash", Patterns: []string{"echo test"}, Tool: opencode.PermissionTool{CallID: callID, MessageID: info.ID}})
		} else {
			f.publish("question.asked", opencode.QuestionRequest{ID: requestID, SessionID: id, Tool: opencode.QuestionTool{CallID: callID, MessageID: info.ID}, Questions: []opencode.QuestionInfo{{Question: "Pick a color", Options: []opencode.QuestionOption{{Label: "blue"}, {Label: "red"}}}}})
		}
		f.mu.Unlock()
		select {
		case <-answer:
		case <-r.Context().Done():
		}
		f.mu.Lock()
		part.State = json.RawMessage(`{"status":"completed","output":"done"}`)
		f.append(id, "message.part.updated", map[string]any{"part": part, "time": time.Now().UnixMilli()})
	}
	output := "hello " + text
	if text == "ENV" {
		carrier, _ := session.Metadata[opencode.CarrierKey].(map[string]any)
		data, _ := json.Marshal(carrier)
		output = string(data)
	}
	info.Time.Completed = time.Now().UnixMilli()
	info.Tokens.Input = 10
	info.Tokens.Output = 3
	if request.Format != nil {
		info.Structured = json.RawMessage(`{"answer":"ok"}`)
	}
	part := opencode.NativePart{ID: opencode.NewID("prt_"), SessionID: id, MessageID: info.ID, Type: "text", Text: output}
	f.append(id, "message.part.updated", map[string]any{"part": part, "time": time.Now().UnixMilli()})
	if text == "IMAGE" {
		path := filepath.Join(session.Directory, "output.png")
		file, err := os.Create(path)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
			panic(err)
		}
		_ = file.Close()
		f.append(id, "message.part.updated", map[string]any{"part": opencode.NativePart{ID: opencode.NewID("prt_"), SessionID: id, MessageID: info.ID, Type: "file", Mime: "image/png", URL: path}, "time": time.Now().UnixMilli()})
	}
	f.append(id, "message.updated", map[string]any{"info": info})
	f.publish("session.status", map[string]any{"sessionID": id, "status": map[string]string{"type": "idle"}})
	fakeWrite(w, opencode.NativeMessage{Info: info, Parts: []opencode.NativePart{part}})
}

//nolint:tagliatelle // The fake accepts the native replay schema.
func (f *fakeOpenCode) replay(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Events []struct {
			ID          string                     `json:"id"`
			AggregateID string                     `json:"aggregateID"`
			Sequence    int64                      `json:"seq"`
			Type        string                     `json:"type"`
			Data        map[string]json.RawMessage `json:"data"`
		} `json:"events"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		w.WriteHeader(400)

		return
	}
	for _, item := range body.Events {
		event := opencode.SyncEvent{ID: item.ID, AggregateID: item.AggregateID, Sequence: item.Sequence, Type: item.Type, Data: item.Data}
		f.rows = append(f.rows, event)
		if strings.HasPrefix(event.Type, "session.") {
			var s opencode.NativeSession
			_ = json.Unmarshal(event.Data["info"], &s)
			f.sessions[s.ID] = s
		}
	}
	f.save()
	fakeWrite(w, true)
}

func (f *fakeOpenCode) agents(w http.ResponseWriter, r *http.Request) {
	var config struct {
		Plugin []string `json:"plugin"`
	}
	_ = json.Unmarshal([]byte(os.Getenv("OPENCODE_CONFIG_CONTENT")), &config)
	for _, plugin := range config.Plugin {
		u, _ := url.Parse(plugin)
		if filepath.Base(u.Path) == "environment.mjs" {
			dir, _ := filepath.EvalSymlinks(r.URL.Query().Get("directory"))
			hash := sha256.Sum256([]byte(dir))
			root := filepath.Join(filepath.Dir(u.Path), "ready")
			_ = os.MkdirAll(root, 0700)
			_ = os.WriteFile(filepath.Join(root, hex.EncodeToString(hash[:])), []byte(dir), 0600)
		}
	}
	fakeWrite(w, []map[string]string{{"name": "build", "mode": "primary"}, {"name": "plan", "mode": "primary"}})
}

func (f *fakeOpenCode) sessionRoute(w http.ResponseWriter, r *http.Request, path string) {
	pieces := strings.Split(strings.Trim(path, "/"), "/")
	if len(pieces) >= 3 && (pieces[0] == "permission" || pieces[0] == "question") {
		if answer := f.answers[pieces[1]]; answer != nil {
			var body json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			answer <- body
			delete(f.answers, pieces[1])
		}
		fakeWrite(w, true)

		return
	}
	if len(pieces) < 2 || pieces[0] != "session" {
		w.WriteHeader(404)

		return
	}
	id := pieces[1]
	session, exists := f.sessions[id]
	if !exists {
		w.WriteHeader(404)

		return
	}
	if len(pieces) == 2 {
		if r.Method == http.MethodPatch {
			var patch struct {
				Metadata map[string]any `json:"metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&patch)
			session.Metadata = patch.Metadata
			f.sessions[id] = session
			f.append(id, "session.updated", map[string]any{"info": session})
		}
		fakeWrite(w, session)

		return
	}
	switch pieces[2] {
	case "message":
		fakeWrite(w, f.messages(id))
	case "abort":
		if pending := f.pending[id]; pending != nil {
			close(pending)
			delete(f.pending, id)
		}
		fakeWrite(w, true)
	default:
		w.WriteHeader(404)
	}
}
