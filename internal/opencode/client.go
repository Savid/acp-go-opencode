package opencode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const MaxBodyBytes = 64 << 20
const EventCapacity = 256

// Client is an authenticated native HTTP endpoint shared by every session.
type Client struct {
	URL      string
	Password string
	http     *http.Client
}

type HTTPError struct {
	Status int
	Native NativeError
}

func (e *HTTPError) Error() string { return fmt.Sprintf("opencode HTTP status %d", e.Status) }
func IsMissing(err error) bool {
	var e *HTTPError

	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func NewClient() (*Client, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return nil, err
	}

	return &Client{URL: "http://" + address, Password: NewID(""), http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func NewID(prefix string) string {
	var value [24]byte

	_, _ = rand.Read(value[:])

	return prefix + hex.EncodeToString(value[:])
}
func (c *Client) Args() []string {
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(c.URL, "http://"))

	return []string{"serve", "--hostname", "127.0.0.1", "--port", port}
}
func (c *Client) request(ctx context.Context, directory, method, path string, body any) (*http.Response, error) {
	var reader io.Reader

	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}

		reader = bytes.NewReader(data)
	}

	target := c.URL + path
	if directory != "" {
		target += "?directory=" + url.QueryEscape(directory)
	}

	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}

	request.SetBasicAuth("opencode", c.Password)
	request.Header.Set("Content-Type", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("opencode request failed: %w", err)
	}

	return response, nil
}
func (c *Client) Do(ctx context.Context, directory, method, path string, body, out any) error {
	response, err := c.request(ctx, directory, method, path, body)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, MaxBodyBytes+1))
	if err != nil {
		return err
	}

	if len(data) > MaxBodyBytes {
		return errors.New("opencode response exceeds size limit")
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		e := &HTTPError{Status: response.StatusCode}
		_ = json.Unmarshal(data, &e.Native)

		return e
	}

	if out == nil || len(data) == 0 {
		return nil
	}

	return json.Unmarshal(data, out)
}
func SessionPath(id string) string { return "/session/" + url.PathEscape(id) }
func (c *Client) Session(ctx context.Context, dir, id string) (NativeSession, error) {
	var out NativeSession

	err := c.Do(ctx, dir, http.MethodGet, SessionPath(id), nil, &out)

	return out, err
}
func (c *Client) Messages(ctx context.Context, dir, id string) ([]NativeMessage, error) {
	var out []NativeMessage

	err := c.Do(ctx, dir, http.MethodGet, SessionPath(id)+"/message", nil, &out)

	return out, err
}
func (c *Client) Interrupt(ctx context.Context, dir, id string) error {
	return c.Do(ctx, dir, http.MethodPost, SessionPath(id)+"/abort", map[string]any{}, nil)
}
func (c *Client) History(ctx context.Context, cursors map[string]int64) ([]SyncEvent, error) {
	var out []SyncEvent

	err := c.Do(ctx, "", http.MethodPost, "/sync/history", cursors, &out)

	return out, err
}
func (c *Client) Replay(ctx context.Context, dir string, events []SyncEvent) error {
	rows := make([]map[string]any, 0, len(events))
	for _, e := range events {
		rows = append(rows, map[string]any{"id": e.ID, "aggregateID": e.AggregateID, "seq": e.Sequence, "type": e.Type, "data": e.Data})
	}

	return c.Do(ctx, dir, http.MethodPost, "/sync/replay", map[string]any{"directory": dir, "events": rows}, nil)
}

// Stream owns one ordered global SSE connection. Closing it joins its reader.
type Stream struct {
	Events chan Event
	done   chan struct{}
	cancel context.CancelFunc
	body   io.ReadCloser
	mu     sync.Mutex
	err    error
}

func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.err
}
func (s *Stream) Close() { s.cancel(); _ = s.body.Close(); <-s.done }

//nolint:bodyclose // The returned stream owns the response body and closes it in its joined reader.
func (c *Client) Subscribe(ctx context.Context) (*Stream, error) {
	streamCtx, cancel := context.WithCancel(ctx)

	response, err := c.request(streamCtx, "", http.MethodGet, "/global/event", nil)
	if err != nil {
		cancel()

		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()

		cancel()

		return nil, errors.New("opencode SSE subscription refused")
	}

	s := &Stream{Events: make(chan Event, EventCapacity), done: make(chan struct{}), cancel: cancel, body: response.Body}
	go func() {
		defer close(s.done)
		defer close(s.Events)
		defer response.Body.Close()

		err := s.read(streamCtx, response.Body)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
	}()

	return s, nil
}
func (s *Stream) read(ctx context.Context, reader io.Reader) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), MaxBodyBytes)

	var data []byte

	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}

		var envelope struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return err
		}

		raw := data
		if len(envelope.Payload) != 0 {
			raw = envelope.Payload
		}

		var event Event
		if err := json.Unmarshal(raw, &event); err != nil {
			return err
		}

		if event.Type == "" {
			return errors.New("native event type missing")
		}

		select {
		case s.Events <- event:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			return errors.New("opencode event queue overflow")
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}

			data = nil

			continue
		}

		if value, ok := strings.CutPrefix(line, "data:"); ok {
			if len(data) > 0 {
				data = append(data, '\n')
			}

			data = append(data, strings.TrimPrefix(value, " ")...)
			if len(data) > MaxBodyBytes {
				return errors.New("opencode event exceeds size limit")
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	if err := dispatch(); err != nil {
		return err
	}

	return io.EOF
}

//nolint:tagliatelle // Native event payloads spell ID in uppercase.
func (e Event) SessionID() string {
	var p struct {
		SessionID string `json:"sessionID"`
		Info      struct {
			SessionID string `json:"sessionID"`
			ID        string `json:"id"`
		} `json:"info"`
		Part struct {
			SessionID string `json:"sessionID"`
		} `json:"part"`
	}
	if json.Unmarshal(e.Properties, &p) != nil {
		return ""
	}

	if p.SessionID != "" {
		return p.SessionID
	}

	if p.Part.SessionID != "" {
		return p.Part.SessionID
	}

	if p.Info.SessionID != "" {
		return p.Info.SessionID
	}

	if strings.HasPrefix(e.Type, "session.") {
		return p.Info.ID
	}

	return ""
}
