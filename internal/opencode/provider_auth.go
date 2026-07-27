package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/savid/acp-go-opencode/internal/homelock"
)

const (
	routeProvider        = "/provider"
	routeProviderAuth    = "/provider/auth"
	routeAuth            = "/auth"
	routeInstanceDispose = "/instance/dispose"

	authStoreDir  = "opencode"
	authStoreFile = "auth.json"

	providerAuthMethodField = "method"
	providerModelsField     = "models"
	providerSourceField     = "source"
)

// providerCatalogAllowlist names every key the adapter reads from a native
// provider catalog entry. The native entry also declares `key`, which carries
// an API key in the clear for env-sourced and injected providers; projecting
// each entry through this list drops it before any decoder can forward it.
var catalogMarshal = json.Marshal

var providerCatalogAllowlist = []string{fieldID, fieldName, "env", providerModelsField, "options", providerSourceField}

// ProviderCatalogEntry is one allowlisted entry of the native `all` provider
// catalog.
type ProviderCatalogEntry struct {
	ID      string                     `json:"id"`
	Name    string                     `json:"name"`
	Source  string                     `json:"source"`
	Env     []string                   `json:"env"`
	Models  map[string]json.RawMessage `json:"models"`
	Options map[string]json.RawMessage `json:"options"`
}

// ProviderAuthMethod is one native login method of a provider. The native
// response carries no id: a method is addressed by its index in this array.
type ProviderAuthMethod struct {
	Type    string               `json:"type"`
	Label   string               `json:"label"`
	Prompts []ProviderAuthPrompt `json:"prompts,omitempty"`
}

// ProviderAuthPrompt is one native input a login method collects.
type ProviderAuthPrompt struct {
	Type        string                     `json:"type"`
	Key         string                     `json:"key"`
	Message     string                     `json:"message"`
	Placeholder string                     `json:"placeholder,omitempty"`
	Options     []ProviderAuthPromptOption `json:"options,omitempty"`
	When        *ProviderAuthPromptWhen    `json:"when,omitempty"`
}

// ProviderAuthPromptOption is one choice of a select prompt.
type ProviderAuthPromptOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Hint  string `json:"hint,omitempty"`
}

// ProviderAuthPromptWhen gates a prompt on an earlier answer.
type ProviderAuthPromptWhen struct {
	Key   string `json:"key"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

// ProviderAuthorization is the native authorize result.
type ProviderAuthorization struct {
	URL          string `json:"url"`
	Method       string `json:"method"`
	Instructions string `json:"instructions"`
}

// ProviderAuthCredential is one entry of the native credential store. It is the
// only native shape carrying credential material, so it decodes strictly.
type ProviderAuthCredential struct {
	Type          string            `json:"type"`
	Refresh       string            `json:"refresh,omitempty"`
	Access        string            `json:"access,omitempty"`
	Expires       int64             `json:"expires,omitempty"`
	AccountID     string            `json:"accountId,omitempty"`
	EnterpriseURL string            `json:"enterpriseUrl,omitempty"`
	Key           string            `json:"key,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// Native credential variants the store may hold. The adapter installs and reads
// the first two; `wellknown` is a native-only shape it never accepts.
const (
	ProviderAuthTypeOAuth = "oauth"
	ProviderAuthTypeAPI   = "api"
)

// ProviderAuthNativeMethod reports which native interaction an authorize result
// selected.
const (
	ProviderAuthNativeMethodAuto = "auto"
	ProviderAuthNativeMethodCode = "code"
)

func (s *openCodeServer) ProviderCatalog(ctx context.Context) ([]ProviderCatalogEntry, error) {
	var response struct {
		All []json.RawMessage `json:"all"`
	}

	if err := s.getJSON(ctx, routeProvider, nil, &response); err != nil {
		return nil, err
	}

	entries := make([]ProviderCatalogEntry, 0, len(response.All))

	for _, raw := range response.All {
		entry, err := decodeProviderCatalogEntry(raw)
		if err != nil {
			return nil, err
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// decodeProviderCatalogEntry projects one native catalog entry through the
// allowlist before it is decoded, so a key outside the list is dropped rather
// than forwarded whether or not the typed shape recognises it. Unknown
// allowlisted-object contents are ignored: a field addition must never break
// enumeration.
func decodeProviderCatalogEntry(raw json.RawMessage) (ProviderCatalogEntry, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ProviderCatalogEntry{}, fmt.Errorf("decode provider catalog entry: %w", err)
	}

	projected := make(map[string]json.RawMessage, len(providerCatalogAllowlist))

	for _, key := range providerCatalogAllowlist {
		if value, ok := fields[key]; ok {
			projected[key] = value
		}
	}

	encoded, err := catalogMarshal(projected)
	if err != nil {
		return ProviderCatalogEntry{}, fmt.Errorf("encode provider catalog entry: %w", err)
	}

	var entry ProviderCatalogEntry
	if err := json.Unmarshal(encoded, &entry); err != nil {
		return ProviderCatalogEntry{}, fmt.Errorf("decode provider catalog entry: %w", err)
	}

	return entry, nil
}

func (s *openCodeServer) ProviderAuthMethods(ctx context.Context) (map[string][]ProviderAuthMethod, error) {
	var raw map[string][]json.RawMessage

	if err := s.getJSON(ctx, routeProviderAuth, nil, &raw); err != nil {
		return nil, err
	}

	methods := make(map[string][]ProviderAuthMethod, len(raw))

	for providerID, entries := range raw {
		decoded := make([]ProviderAuthMethod, 0, len(entries))

		for index, entry := range entries {
			method, err := decodeProviderAuthMethod(entry)
			if err != nil {
				return nil, fmt.Errorf("provider %s method %d: %w", providerID, index, err)
			}

			decoded = append(decoded, method)
		}

		methods[providerID] = decoded
	}

	return methods, nil
}

// decodeProviderAuthMethod ignores unknown keys on the method itself, which is
// catalog identity, and decodes its prompts strictly: a prompt schema this
// build cannot represent must fail enumeration rather than reach a caller
// partially decoded.
func decodeProviderAuthMethod(raw json.RawMessage) (ProviderAuthMethod, error) {
	var envelope struct {
		Type    string            `json:"type"`
		Label   string            `json:"label"`
		Prompts []json.RawMessage `json:"prompts"`
	}

	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ProviderAuthMethod{}, fmt.Errorf("decode auth method: %w", err)
	}

	method := ProviderAuthMethod{Type: envelope.Type, Label: envelope.Label}

	for _, promptRaw := range envelope.Prompts {
		prompt, err := decodeProviderAuthPrompt(promptRaw)
		if err != nil {
			return ProviderAuthMethod{}, err
		}

		method.Prompts = append(method.Prompts, prompt)
	}

	return method, nil
}

func decodeProviderAuthPrompt(raw json.RawMessage) (ProviderAuthPrompt, error) {
	var prompt ProviderAuthPrompt
	if err := strictDecode(raw, &prompt); err != nil {
		return ProviderAuthPrompt{}, fmt.Errorf("decode auth prompt: %w", err)
	}

	return prompt, nil
}

func strictDecode(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(out); err != nil {
		return err
	}

	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing content")
	}

	return nil
}

func (s *openCodeServer) ProviderAuthorize(ctx context.Context, providerID string, method int, inputs map[string]string) (ProviderAuthorization, error) {
	body := map[string]any{providerAuthMethodField: method}
	if len(inputs) > 0 {
		body["inputs"] = inputs
	}

	var out ProviderAuthorization

	err := s.doJSON(ctx, http.MethodPost, providerOAuthPath(providerID, "authorize"), nil, body, &out)

	return out, err
}

// ProviderAuthCallback completes a native OAuth flow. The native endpoint
// blocks until the provider settles the flow, so it runs on a client with no
// request timeout and is bounded by the caller's context alone.
func (s *openCodeServer) ProviderAuthCallback(ctx context.Context, providerID string, method int, code string) error {
	body := map[string]any{providerAuthMethodField: method}
	if code != "" {
		body["code"] = code
	}

	return s.doJSONWithClient(ctx, s.blockingHTTPClient(), http.MethodPost, providerOAuthPath(providerID, "callback"), nil, body, nil)
}

func providerOAuthPath(providerID string, leg string) string {
	return routeProvider + "/" + urlPathSegment(providerID) + "/oauth/" + leg
}

func (s *openCodeServer) SetProviderAuth(ctx context.Context, providerID string, credential ProviderAuthCredential) error {
	return s.doJSON(ctx, http.MethodPut, routeAuth+"/"+urlPathSegment(providerID), nil, credential, nil)
}

func (s *openCodeServer) RemoveProviderAuth(ctx context.Context, providerID string) error {
	return s.doJSON(ctx, http.MethodDelete, routeAuth+"/"+urlPathSegment(providerID), nil, nil, nil)
}

// DisposeInstance releases the server's cached instance so a credential written
// through SetProviderAuth is visible to later work without a restart.
func (s *openCodeServer) DisposeInstance(ctx context.Context) error {
	return s.doJSON(ctx, http.MethodPost, routeInstanceDispose, nil, nil, nil)
}

// StoredProviderAuth reads one entry of this server's own credential store.
// There is no native route that returns a stored credential, so residence is
// answered by reading the store file inside the root this server owns.
func (s *openCodeServer) StoredProviderAuth(_ context.Context, providerID string) (ProviderAuthCredential, bool, error) {
	return readProviderAuthStore(s.xdg.Data, providerID)
}

var authStoreReadFile = os.ReadFile

func readProviderAuthStore(dataDir string, providerID string) (ProviderAuthCredential, bool, error) {
	contents, err := authStoreReadFile(filepath.Join(dataDir, authStoreDir, authStoreFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ProviderAuthCredential{}, false, nil
		}

		return ProviderAuthCredential{}, false, fmt.Errorf("read auth store: %w", err)
	}

	var store map[string]json.RawMessage
	if err := json.Unmarshal(contents, &store); err != nil {
		return ProviderAuthCredential{}, false, fmt.Errorf("decode auth store: %w", err)
	}

	raw, ok := store[providerID]
	if !ok {
		return ProviderAuthCredential{}, false, nil
	}

	var credential ProviderAuthCredential
	if err := strictDecode(raw, &credential); err != nil {
		return ProviderAuthCredential{}, false, fmt.Errorf("decode stored credential: %w", err)
	}

	switch {
	case credential.Type == ProviderAuthTypeOAuth && credential.Refresh != "" && credential.Access != "":
	case credential.Type == ProviderAuthTypeAPI && credential.Key != "":
	default:
		return ProviderAuthCredential{}, false, fmt.Errorf("unsupported stored credential type %q", credential.Type)
	}

	return credential, true, nil
}

func urlPathSegment(value string) string {
	return url.PathEscape(value)
}

// IsRateLimited reports a native refusal that asks the caller to slow down.
func IsRateLimited(err error) bool {
	return isHTTPStatus(err, http.StatusTooManyRequests)
}

// ReapAbandonedHomes removes every prefixed home under parent whose runtime
// locks are free. A crashed adapter otherwise leaks an orphan server still
// holding a pending flow, and a home a live server still owns keeps its locks
// and is left alone.
var (
	reapReadDir   = os.ReadDir
	reapAcquire   = homelock.Acquire
	reapRemoveAll = os.RemoveAll
)

func ReapAbandonedHomes(parent string, prefix string) error {
	entries, err := reapReadDir(parent)
	if err != nil {
		return fmt.Errorf("scan scratch parent: %w", err)
	}

	var errs []error

	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}

		home := filepath.Join(parent, entry.Name())

		lock, err := reapAcquire(home)
		if err != nil {
			continue
		}

		errs = append(errs, lock.Release(), reapRemoveAll(home))
	}

	return errors.Join(errs...)
}
