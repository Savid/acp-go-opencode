package opencodeacp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestSessionCLIHelpers(t *testing.T) {
	restore := stubOpenCodeCommand(t)
	defer restore()

	exported, err := ExportSession(context.Background(), "ses_test", helperOptions()...)
	if err != nil {
		t.Fatalf("ExportSession returned error: %v", err)
	}
	if !json.Valid(exported) || strings.Contains(string(exported), "Exporting session") {
		t.Fatalf("exported JSON = %q", exported)
	}

	sessionID, err := SessionIDFromExport(exported)
	if err != nil {
		t.Fatalf("SessionIDFromExport returned error: %v", err)
	}
	if sessionID != "ses_test" {
		t.Fatalf("session id = %q", sessionID)
	}

	prefixedID, err := SessionIDFromExport(append([]byte("status\n"), exported...))
	if err != nil {
		t.Fatalf("SessionIDFromExport prefixed returned error: %v", err)
	}
	if prefixedID != "ses_test" {
		t.Fatalf("prefixed session id = %q", prefixedID)
	}

	latest, err := ExportSession(context.Background(), "", helperOptions()...)
	if err != nil {
		t.Fatalf("ExportSession latest returned error: %v", err)
	}
	latestID, err := SessionIDFromExport(latest)
	if err != nil {
		t.Fatalf("latest SessionIDFromExport returned error: %v", err)
	}
	if latestID != "ses_latest" {
		t.Fatalf("latest session id = %q", latestID)
	}

	importOut, err := ImportSessionFile(context.Background(), "/tmp/session.json", helperOptions()...)
	if err != nil {
		t.Fatalf("ImportSessionFile returned error: %v", err)
	}
	if !strings.Contains(string(importOut), "Imported /tmp/session.json") {
		t.Fatalf("import output = %q", importOut)
	}

	if err := DeleteSession(context.Background(), "ses_test", helperOptions()...); err != nil {
		t.Fatalf("DeleteSession returned error: %v", err)
	}
}

func TestSessionCLIHelperErrors(t *testing.T) {
	restore := stubOpenCodeCommand(t)
	defer restore()

	for _, tc := range []struct {
		name string
		run  func() error
		want string
	}{
		{
			name: "export command error",
			run: func() error {
				opts := append(helperOptions(), WithEnv(map[string]string{
					helperEnvKey:              "1",
					"OPENCODEACP_HELPER_MODE": "fail-export",
				}))
				_, err := ExportSession(context.Background(), acp.SessionId("ses_test"), opts...)

				return err
			},
			want: "export failed",
		},
		{
			name: "empty import path",
			run: func() error {
				_, err := ImportSessionFile(context.Background(), " ", helperOptions()...)

				return err
			},
			want: "import path is empty",
		},
		{
			name: "import command error",
			run: func() error {
				opts := append(helperOptions(), WithEnv(map[string]string{
					helperEnvKey:              "1",
					"OPENCODEACP_HELPER_MODE": "fail-import",
				}))
				_, err := ImportSessionFile(context.Background(), "/tmp/session.json", opts...)

				return err
			},
			want: "import failed",
		},
		{
			name: "empty delete session id",
			run: func() error {
				return DeleteSession(context.Background(), "", helperOptions()...)
			},
			want: "session id is empty",
		},
		{
			name: "delete command error",
			run: func() error {
				opts := append(helperOptions(), WithEnv(map[string]string{
					helperEnvKey:              "1",
					"OPENCODEACP_HELPER_MODE": "fail-delete",
				}))

				return DeleteSession(context.Background(), "ses_test", opts...)
			},
			want: "delete failed",
		},
		{
			name: "export no json",
			run: func() error {
				opts := append(helperOptions(), WithEnv(map[string]string{
					helperEnvKey:              "1",
					"OPENCODEACP_HELPER_MODE": "export-no-json",
				}))
				_, err := ExportSession(context.Background(), acp.SessionId("ses_test"), opts...)

				return err
			},
			want: "did not contain JSON",
		},
		{
			name: "export invalid json",
			run: func() error {
				opts := append(helperOptions(), WithEnv(map[string]string{
					helperEnvKey:              "1",
					"OPENCODEACP_HELPER_MODE": "export-invalid-json",
				}))
				_, err := ExportSession(context.Background(), acp.SessionId("ses_test"), opts...)

				return err
			},
			want: "invalid JSON",
		},
		{
			name: "session id parse error",
			run: func() error {
				_, err := SessionIDFromExport([]byte(`{"info":[]}`))

				return err
			},
			want: "parse opencode export JSON",
		},
		{
			name: "import output error",
			run: func() error {
				opts := append(helperOptions(), WithEnv(map[string]string{
					helperEnvKey:              "1",
					"OPENCODEACP_HELPER_MODE": "import-output-error",
				}))
				_, err := ImportSessionFile(context.Background(), "/tmp/session.json", opts...)

				return err
			},
			want: "Missing key",
		},
		{
			name: "delete output error",
			run: func() error {
				opts := append(helperOptions(), WithEnv(map[string]string{
					helperEnvKey:              "1",
					"OPENCODEACP_HELPER_MODE": "delete-output-error",
				}))

				return DeleteSession(context.Background(), "ses_test", opts...)
			},
			want: "Session not found",
		},
		{
			name: "session id malformed json",
			run: func() error {
				_, err := SessionIDFromExport([]byte(`{"info":`))

				return err
			},
			want: "invalid JSON",
		},
		{
			name: "session id missing",
			run: func() error {
				_, err := SessionIDFromExport([]byte(`{"info":{}}`))

				return err
			},
			want: "missing info.id",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	if err := openCodeOutputError("opencode test", nil); err != nil {
		t.Fatalf("empty output error = %v", err)
	}
	if err := openCodeOutputError("opencode test", []byte("ok")); err != nil {
		t.Fatalf("ok output error = %v", err)
	}
}
