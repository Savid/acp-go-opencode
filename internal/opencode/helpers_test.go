package opencode

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestPermissionRequestRoute(t *testing.T) {
	cases := []struct {
		name string
		req  PermissionRequest
		want PermissionRoute
	}{
		{"reply route wins", PermissionRequest{ReplyRoute: PermissionRouteSession, Action: "edit"}, PermissionRouteSession},
		{"action means api", PermissionRequest{Action: "edit"}, PermissionRouteAPI},
		{"no action means session", PermissionRequest{}, PermissionRouteSession},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.req.Route(); got != tc.want {
				t.Fatalf("Route() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPermissionRequestActionNameAndResources(t *testing.T) {
	if got := (PermissionRequest{Action: "edit"}).ActionName(); got != "edit" {
		t.Fatalf("ActionName action = %q", got)
	}

	if got := (PermissionRequest{Permission: "ask"}).ActionName(); got != "ask" {
		t.Fatalf("ActionName permission fallback = %q", got)
	}

	if got := (PermissionRequest{Resources: []string{"a"}}).ResourceList(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("ResourceList resources = %#v", got)
	}

	if got := (PermissionRequest{Patterns: []string{"b"}}).ResourceList(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("ResourceList patterns fallback = %#v", got)
	}
}

func TestQuestionRequestRoute(t *testing.T) {
	if got := (QuestionRequest{ReplyRoute: QuestionRouteAPI}).Route(); got != QuestionRouteAPI {
		t.Fatalf("Route reply = %q", got)
	}

	if got := (QuestionRequest{}).Route(); got != QuestionRouteSession {
		t.Fatalf("Route default = %q", got)
	}
}

func TestProvidersResponseHasModel(t *testing.T) {
	providers := ProvidersResponse{Providers: []ProviderInfo{{
		ID:     "openai",
		Models: map[string]ProviderModel{"gpt-test": {ID: "gpt-test"}},
	}}}

	if !providers.HasModel("openai/gpt-test") {
		t.Fatal("HasModel should match provider/model")
	}

	if providers.HasModel("openai/missing") {
		t.Fatal("HasModel should not match unknown model")
	}

	if providers.HasModel("no-slash") {
		t.Fatal("HasModel should reject value without a slash")
	}

	if providers.HasModel("other/gpt-test") {
		t.Fatal("HasModel should not match a different provider id")
	}
}

func TestIsBadRequest(t *testing.T) {
	badRequest := &HTTPError{StatusCode: http.StatusBadRequest}
	if !IsBadRequest(badRequest) {
		t.Fatal("IsBadRequest should match 400")
	}

	if IsBadRequest(&HTTPError{StatusCode: http.StatusInternalServerError}) {
		t.Fatal("IsBadRequest should not match 500")
	}

	if IsBadRequest(errors.New("other")) {
		t.Fatal("IsBadRequest should not match non-HTTP error")
	}
}

func TestStreamErrorEpoch(t *testing.T) {
	if got := StreamErrorEpoch(StreamError{Epoch: 5, Err: errors.New("x")}); got != 5 {
		t.Fatalf("StreamErrorEpoch = %d, want 5", got)
	}

	if got := StreamErrorEpoch(errors.New("plain")); got != 0 {
		t.Fatalf("StreamErrorEpoch plain = %d, want 0", got)
	}
}

func TestIntFromNumber(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  int
		ok    bool
	}{
		{"float64", float64(3), 3, true},
		{"float64 negative", float64(-1), 0, false},
		{"int", 4, 4, true},
		{"int negative", -2, 0, false},
		{"json number", json.Number("6"), 6, true},
		{"json number invalid", json.Number("nope"), 0, false},
		{"other type", "str", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := IntFromNumber(tc.value)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("IntFromNumber(%v) = (%d,%t), want (%d,%t)", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
}
