package opencode

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const sessionCarrierRoute = "/carrier/"

type sessionCarrierPayload struct {
	Env           map[string]string `json:"env"`
	ExtraPathDirs []string          `json:"extraPathDirs"`
}

type sessionCarrierBroker struct {
	server   *http.Server
	listener net.Listener
	endpoint string
	token    string

	mu        sync.RWMutex
	carriers  map[string]sessionCarrierPayload
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

var sessionCarrierListen = net.Listen

func startSessionCarrierBroker() (*sessionCarrierBroker, error) {
	token, err := randomPassword()
	if err != nil {
		return nil, fmt.Errorf("generate OpenCode session carrier broker authorization: %w", err)
	}

	listener, err := sessionCarrierListen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for OpenCode session carrier: %w", err)
	}

	broker := &sessionCarrierBroker{
		listener: listener,
		endpoint: "http://" + listener.Addr().String(),
		token:    token,
		carriers: map[string]sessionCarrierPayload{},
	}
	broker.server = &http.Server{
		Handler:           http.HandlerFunc(broker.serveHTTP),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	go func() {
		_ = broker.server.Serve(listener)
	}()

	return broker, nil
}

func (b *sessionCarrierBroker) put(payload sessionCarrierPayload) (string, error) {
	if b == nil {
		return "", errors.New("session carrier broker is unavailable")
	}

	reference, err := randomPassword()
	if err != nil {
		return "", err
	}

	payload.Env = maps.Clone(payload.Env)
	if payload.Env == nil {
		payload.Env = map[string]string{}
	}

	payload.ExtraPathDirs = append([]string{}, payload.ExtraPathDirs...)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()

		return "", errors.New("session carrier broker is closed")
	}

	b.carriers[reference] = payload
	b.mu.Unlock()

	return reference, nil
}

func (b *sessionCarrierBroker) remove(reference string) {
	if b == nil || reference == "" {
		return
	}

	b.mu.Lock()
	delete(b.carriers, reference)
	b.mu.Unlock()
}

func (b *sessionCarrierBroker) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)

		return
	}

	authorization, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(authorization), []byte(b.token)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	reference, ok := strings.CutPrefix(r.URL.Path, sessionCarrierRoute)
	if !ok || reference == "" || strings.Contains(reference, "/") {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	b.mu.RLock()
	payload, ok := b.carriers[reference]
	b.mu.RUnlock()

	if !ok {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		return
	}
}

func (b *sessionCarrierBroker) Close() error {
	if b == nil {
		return nil
	}

	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		clear(b.carriers)
		b.mu.Unlock()

		b.closeErr = b.server.Close()
	})

	return b.closeErr
}
