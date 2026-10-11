package opencode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadAddress(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		output string
		want   string
	}{
		{name: "native address after noise", output: "startup notice\nopencode server listening on http://127.0.0.1:49152\n", want: "http://127.0.0.1:49152"},
		{name: "missing address", output: "startup failed\n"},
		{name: "unbound port", output: "opencode server listening on http://127.0.0.1:0\n"},
		{name: "foreign address", output: "opencode server listening on http://192.0.2.1:49152\n"},
		{name: "URL credentials", output: "opencode server listening on http://user:password@127.0.0.1:49152\n"},
		{name: "oversized line", output: strings.Repeat("x", 64<<10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := NewClient()
			err := client.ReadAddress(strings.NewReader(tc.output))
			if tc.want == "" {
				require.Error(t, err)
				require.Empty(t, client.URL)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, client.URL)
			}
		})
	}
}

func TestTurnMessagesPagesBackToTheTurn(t *testing.T) {
	t.Parallel()

	messages := make([]NativeMessage, 70)
	for index := range messages {
		messages[index].Info.ID = fmt.Sprintf("msg_%03d", index)
	}

	var pages atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)

		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit != turnPageSize || r.URL.Query().Get("directory") != "/work" {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		end := len(messages)
		if before := r.URL.Query().Get("before"); before != "" {
			end = slices.IndexFunc(messages, func(message NativeMessage) bool { return message.Info.ID == before })
		}

		start := max(end-limit, 0)
		if start > 0 {
			w.Header().Set("X-Next-Cursor", messages[start].Info.ID)
		}

		_ = json.NewEncoder(w).Encode(messages[start:end])
	}))
	t.Cleanup(server.Close)

	client := NewClient()
	client.URL = server.URL

	for _, tc := range []struct {
		name      string
		messageID string
		first     int
		pages     int32
	}{
		{name: "latest page", messageID: "msg_060", first: 60, pages: 1},
		{name: "older page", messageID: "msg_020", first: 20, pages: 2},
		{name: "absent", messageID: "msg_missing", first: 0, pages: 3},
	} {
		pages.Store(0)

		got, err := client.TurnMessages(t.Context(), "/work", "ses", tc.messageID)
		require.NoError(t, err, tc.name)
		require.Equal(t, messages[tc.first:], got, tc.name)
		require.Equal(t, tc.pages, pages.Load(), tc.name)
	}
}

func TestStreamDecodesEachFrameOnce(t *testing.T) {
	t.Parallel()

	frames := strings.Join([]string{
		`: connected`,
		``,
		`data: {"directory":"/w","payload":{"id":"evt_1","type":"sync","syncEvent":{"id":"evt_0","aggregateID":"ses_a","seq":0,"type":"session.created.1","data":{}}}}`,
		``,
		`data: {"directory":"/w","payload":{"id":"evt_2","type":"message.part.updated","properties":{"part":{"sessionID":"ses_a","id":"prt"}}}}`,
		``,
		"data: {\"directory\":\"/w\",\r",
		`data: "payload":{"id":"evt_3","type":"session.updated","properties":{"info":{"id":"ses_b"}}}}`,
		``,
		`data: {"payload":{"id":"evt_4","type":"server.heartbeat","properties":{}}}`,
		``,
		`data: {"directory":"/w","payload":{"id":"evt_5","properties":{}}}`,
		``,
	}, "\n")

	stream := &Stream{Events: make(chan Event, EventCapacity)}
	require.EqualError(t, stream.read(t.Context(), strings.NewReader(frames)), "native event type missing", "a frame without an event type ends the stream")
	close(stream.Events)

	var got []Event
	for event := range stream.Events {
		got = append(got, event)
	}

	require.Len(t, got, 3, "the sync mirror of a durable event carries no bus event")
	require.Equal(t, "evt_2", got[0].ID)
	require.Equal(t, "ses_a", got[0].SessionID)
	require.JSONEq(t, `{"part":{"sessionID":"ses_a","id":"prt"}}`, string(got[0].Properties))
	require.Equal(t, "ses_b", got[1].SessionID, "a multi-line frame joins its data lines")
	require.Empty(t, got[2].SessionID)
}
