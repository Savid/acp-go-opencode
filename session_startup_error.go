package opencodeacp

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	sessionStartupErrorTag              = "opencode_session_start_failed"
	jsonFieldHTTP                       = "http"
	jsonFieldPathClass                  = "pathClass"
	sessionStartupPathRuntimeHealth     = "runtime_health"
	sessionStartupPathRuntimeContract   = "runtime_contract"
	sessionStartupPathRuntimeConfig     = "runtime_configuration"
	sessionStartupPathMCPRegistration   = "mcp_registration"
	sessionStartupPathProviderCatalog   = "provider_catalog"
	sessionStartupPathSessionCollection = "session_collection"
	sessionStartupRouteRuntimeHealth    = "/global/health"
	sessionStartupRouteRuntimeContract  = "/doc"
	sessionStartupRouteRuntimeConfig    = "/config"
	sessionStartupRouteMCP              = "/mcp"
	sessionStartupRouteProviderCatalog  = "/config/providers"
	sessionStartupRouteSession          = "/session"
)

// SessionStartupStage identifies a values-free session startup boundary.
type SessionStartupStage string

const (
	SessionStartupRuntimeStart        SessionStartupStage = "runtime_start"
	SessionStartupCarrierProof        SessionStartupStage = "carrier_proof"
	SessionStartupScope               SessionStartupStage = "scope"
	SessionStartupCatalog             SessionStartupStage = "catalog"
	SessionStartupNativeSessionCreate SessionStartupStage = "native_session_create"
)

// SessionStartupHTTPFailure identifies an allowlisted loopback HTTP operation.
type SessionStartupHTTPFailure struct {
	Method     string `json:"method"`
	PathClass  string `json:"pathClass"`
	StatusCode int    `json:"statusCode"`
}

// SessionStartupFailure is the safe diagnostic projection of a startup error.
type SessionStartupFailure struct {
	Stage SessionStartupStage        `json:"stage"`
	HTTP  *SessionStartupHTTPFailure `json:"http,omitempty"`
}

type sessionStartupError struct {
	failure SessionStartupFailure
	err     error
}

func (e *sessionStartupError) Error() string { return e.err.Error() }

func (e *sessionStartupError) Unwrap() error { return e.err }

func wrapSessionStartupError(stage SessionStartupStage, err error) error {
	if err == nil {
		return nil
	}

	var existing *sessionStartupError
	if errors.As(err, &existing) {
		return err
	}

	var requestErr *acp.RequestError
	if errors.As(err, &requestErr) || errors.Is(err, context.Canceled) {
		return err
	}

	return &sessionStartupError{
		failure: SessionStartupFailure{Stage: stage, HTTP: safeSessionStartupHTTP(stage, err)},
		err:     err,
	}
}

func safeSessionStartupHTTP(stage SessionStartupStage, err error) *SessionStartupHTTPFailure {
	var httpErr *opencode.HTTPError
	if !errors.As(err, &httpErr) {
		return nil
	}

	pathClass := safeSessionStartupHTTPPathClass(stage, httpErr.Method, httpErr.Path)

	if pathClass == "" || !sessionStartupHTTPFailureStatus(httpErr.StatusCode) {
		return nil
	}

	return &SessionStartupHTTPFailure{
		Method:     httpErr.Method,
		PathClass:  pathClass,
		StatusCode: httpErr.StatusCode,
	}
}

func safeSessionStartupHTTPPathClass(stage SessionStartupStage, method, path string) string {
	switch stage {
	case SessionStartupRuntimeStart:
		switch {
		case method == http.MethodGet && path == sessionStartupRouteRuntimeHealth:
			return sessionStartupPathRuntimeHealth
		case method == http.MethodGet && path == sessionStartupRouteRuntimeContract:
			return sessionStartupPathRuntimeContract
		}
	case SessionStartupCarrierProof:
		if method == http.MethodGet && path == sessionStartupRouteRuntimeConfig {
			return sessionStartupPathRuntimeConfig
		}
	case SessionStartupScope:
		switch {
		case method == http.MethodPost && path == sessionStartupRouteMCP:
			return sessionStartupPathMCPRegistration
		case method == http.MethodDelete && strings.HasPrefix(path, sessionStartupRouteMCP+"/") && !strings.ContainsAny(path, "?#"):
			return sessionStartupPathMCPRegistration
		}
	case SessionStartupCatalog:
		if method == http.MethodGet && path == sessionStartupRouteProviderCatalog {
			return sessionStartupPathProviderCatalog
		}
	case SessionStartupNativeSessionCreate:
		if method == http.MethodPost && path == sessionStartupRouteSession {
			return sessionStartupPathSessionCollection
		}
	}

	return ""
}

func sessionStartupHTTPFailureStatus(statusCode int) bool {
	return statusCode >= 100 && statusCode <= 599 && (statusCode < 200 || statusCode >= 300)
}

func safeSessionStartupHTTPTuple(stage SessionStartupStage, method, pathClass string) bool {
	switch stage {
	case SessionStartupRuntimeStart:
		return method == http.MethodGet &&
			(pathClass == sessionStartupPathRuntimeHealth || pathClass == sessionStartupPathRuntimeContract)
	case SessionStartupCarrierProof:
		return method == http.MethodGet && pathClass == sessionStartupPathRuntimeConfig
	case SessionStartupScope:
		return pathClass == sessionStartupPathMCPRegistration &&
			(method == http.MethodPost || method == http.MethodDelete)
	case SessionStartupCatalog:
		return method == http.MethodGet && pathClass == sessionStartupPathProviderCatalog
	case SessionStartupNativeSessionCreate:
		return method == http.MethodPost && pathClass == sessionStartupPathSessionCollection
	}

	return false
}

func sessionStartupErrorData(err error) (map[string]any, bool) {
	failure, ok := SessionStartupFailureFromError(err)
	if !ok {
		return nil, false
	}

	data := map[string]any{
		jsonFieldError: sessionStartupErrorTag,
		jsonFieldStage: string(failure.Stage),
	}
	if failure.HTTP != nil {
		data[jsonFieldHTTP] = map[string]any{
			jsonFieldMethod:     failure.HTTP.Method,
			jsonFieldPathClass:  failure.HTTP.PathClass,
			jsonFieldStatusCode: failure.HTTP.StatusCode,
		}
	}

	return data, true
}

// SessionStartupFailureFromError reads either an in-process startup error or
// its ACP RequestError projection.
func SessionStartupFailureFromError(err error) (SessionStartupFailure, bool) {
	var requestErr *acp.RequestError
	if errors.As(err, &requestErr) {
		return sessionStartupFailureFromRequestError(requestErr)
	}

	if errors.Is(err, context.Canceled) {
		return SessionStartupFailure{}, false
	}

	var startupErr *sessionStartupError
	if errors.As(err, &startupErr) {
		return cloneSessionStartupFailure(startupErr.failure), true
	}

	return SessionStartupFailure{}, false
}

func sessionStartupFailureFromRequestError(requestErr *acp.RequestError) (SessionStartupFailure, bool) {
	if requestErr == nil || requestErr.Code != -32603 {
		return SessionStartupFailure{}, false
	}

	data, ok := requestErr.Data.(map[string]any)
	if !ok || data[jsonFieldError] != sessionStartupErrorTag {
		return SessionStartupFailure{}, false
	}

	stage, ok := sessionStartupStage(data[jsonFieldStage])
	if !ok {
		return SessionStartupFailure{}, false
	}

	failure := SessionStartupFailure{Stage: stage}

	if rawHTTP, exists := data[jsonFieldHTTP]; exists {
		httpFailure, valid := sessionStartupHTTPFailure(stage, rawHTTP)
		if !valid {
			return SessionStartupFailure{}, false
		}

		failure.HTTP = &httpFailure
	}

	return failure, true
}

func sessionStartupStage(value any) (SessionStartupStage, bool) {
	stage, ok := value.(string)
	if !ok {
		return "", false
	}

	switch SessionStartupStage(stage) {
	case SessionStartupRuntimeStart, SessionStartupCarrierProof, SessionStartupScope,
		SessionStartupCatalog, SessionStartupNativeSessionCreate:
		return SessionStartupStage(stage), true
	default:
		return "", false
	}
}

func sessionStartupHTTPFailure(stage SessionStartupStage, value any) (SessionStartupHTTPFailure, bool) {
	fields, ok := value.(map[string]any)
	if !ok {
		return SessionStartupHTTPFailure{}, false
	}

	method, methodOK := fields[jsonFieldMethod].(string)
	pathClass, pathOK := fields[jsonFieldPathClass].(string)
	statusCode, statusOK := integerField(fields[jsonFieldStatusCode])

	if !methodOK || !pathOK || !statusOK || !safeSessionStartupHTTPTuple(stage, method, pathClass) ||
		!sessionStartupHTTPFailureStatus(statusCode) {
		return SessionStartupHTTPFailure{}, false
	}

	return SessionStartupHTTPFailure{Method: method, PathClass: pathClass, StatusCode: statusCode}, true
}

func integerField(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case float64:
		converted := int(number)

		return converted, float64(converted) == number
	default:
		return 0, false
	}
}

func cloneSessionStartupFailure(failure SessionStartupFailure) SessionStartupFailure {
	if failure.HTTP == nil {
		return failure
	}

	httpFailure := *failure.HTTP
	failure.HTTP = &httpFailure

	return failure
}
