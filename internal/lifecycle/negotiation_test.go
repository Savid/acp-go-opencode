package lifecycle

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeCapabilityReadsExactVersion(t *testing.T) {
	t.Parallel()
	for _, version := range []any{1, 1.0, json.Number("1")} {
		present, refusal := DecodeCapability(map[string]any{MetaKey: map[string]any{"version": version}})
		require.Nil(t, refusal)
		require.True(t, present)
	}
}

// TestDecodeCapabilityTreatsAbsenceAsNoRequest proves an absent key is the host asking
// for nothing rather than a refusal.
func TestDecodeCapabilityTreatsAbsenceAsNoRequest(t *testing.T) {
	t.Parallel()

	present, refusal := DecodeCapability(map[string]any{})
	require.Nil(t, refusal)
	require.False(t, present)
}

func TestDecodeCapabilityStrictness(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		value any
		field string
	}{
		{"not an object", []any{1}, MetaPath},
		{"unknown member", map[string]any{"version": 1, "extra": true}, MetaPath + ".extra"},
		{"version missing", map[string]any{}, MetaPath + ".version"},
		{"other integer", map[string]any{"version": 2}, MetaPath + ".version"},
		{"version not an integer", map[string]any{"version": "1"}, MetaPath + ".version"},
		{"version fractional", map[string]any{"version": 1.5}, MetaPath + ".version"},
		{"version boolean", map[string]any{"version": true}, MetaPath + ".version"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			present, refusal := DecodeCapability(map[string]any{MetaKey: row.value})
			require.False(t, present)
			require.NotNil(t, refusal)
			require.Equal(t, row.field, refusal.Field)
			require.Equal(t, "unsupported "+row.field, refusal.Error())
		})
	}
}

func TestNegotiatedAdvertisementShape(t *testing.T) {
	t.Parallel()

	degenerate := Negotiated{Version: 1}
	require.Equal(t, map[string]any{
		"version":                 1,
		"updatesOutsidePrompt":    false,
		"authoritativeQuiescence": false,
		"activityKinds":           []string{},
	}, degenerate.Advertisement())

	proven := Negotiated{
		Version:                 1,
		AuthoritativeQuiescence: true,
		QuiescenceSource:        ProofClassProcessContainment,
		ActivityKinds:           []ActivityKind{ActivityTask},
	}
	require.Equal(t, "process-containment", proven.Advertisement()["quiescenceSource"])
	require.Equal(t, []string{"task"}, proven.Advertisement()["activityKinds"])
	require.True(t, proven.DeclaresActivityKind(ActivityTask))
	require.False(t, proven.DeclaresActivityKind(ActivitySubagent))

	var absent Negotiated
	require.False(t, absent.Present())
}

// TestRefuseKeyNamesTheExactPath proves a surface that carries no lifecycle value
// refuses the reserved literal by path rather than ignoring it.
func TestRefuseKeyNamesTheExactPath(t *testing.T) {
	t.Parallel()

	require.Nil(t, RefuseKey(nil))
	require.Nil(t, RefuseKey(map[string]any{"acp-go.dev/route": map[string]any{}}))

	refusal := RefuseKey(map[string]any{MetaKey: map[string]any{}})
	require.NotNil(t, refusal)
	require.Equal(t, MetaPath, refusal.Field)
}

func TestDecodePromptCorrelation(t *testing.T) {
	t.Parallel()

	negotiated := Negotiated{Version: 1}

	submission, refusal := DecodePromptCorrelation(map[string]any{MetaKey: map[string]any{
		"version":    1,
		"submission": map[string]any{"submissionId": "sub-1", "clientNonce": "nonce-1", "runId": "run-1"},
	}}, negotiated)
	require.Nil(t, refusal)
	require.Equal(t, Submission{SubmissionID: "sub-1", ClientNonce: "nonce-1", RunID: "run-1"}, submission)
}

// TestDecodePromptCorrelationUnnegotiated proves the key is forbidden while the
// extension is not negotiated and absent is then the correct shape.
func TestDecodePromptCorrelationUnnegotiated(t *testing.T) {
	t.Parallel()

	_, refusal := DecodePromptCorrelation(map[string]any{MetaKey: map[string]any{}}, Negotiated{})
	require.NotNil(t, refusal)
	require.Equal(t, MetaPath, refusal.Field)

	submission, refusal := DecodePromptCorrelation(map[string]any{}, Negotiated{})
	require.Nil(t, refusal)
	require.Equal(t, Submission{}, submission)
}

func TestDecodePromptCorrelationStrictness(t *testing.T) {
	t.Parallel()

	valid := map[string]any{"submissionId": "sub-1", "clientNonce": "nonce-1"}

	for _, row := range []struct {
		name  string
		meta  map[string]any
		field string
	}{
		{"key missing", map[string]any{}, MetaPath},
		{"not an object", map[string]any{MetaKey: []any{}}, MetaPath},
		{"unknown member", map[string]any{MetaKey: map[string]any{"version": 1, "submission": valid, "extra": 1}}, MetaPath + ".extra"},
		{"version missing", map[string]any{MetaKey: map[string]any{"submission": valid}}, MetaPath + ".version"},
		{"version unsupported", map[string]any{MetaKey: map[string]any{"version": 2, "submission": valid}}, MetaPath + ".version"},
		{"version fractional", map[string]any{MetaKey: map[string]any{"version": 1.5, "submission": valid}}, MetaPath + ".version"},
		{"version beyond every int", map[string]any{MetaKey: map[string]any{"version": 1e300, "submission": valid}}, MetaPath + ".version"},
		{"submission missing", map[string]any{MetaKey: map[string]any{"version": 1}}, MetaPath + ".submission"},
		{"submission unknown member", map[string]any{MetaKey: map[string]any{"version": 1, "submission": map[string]any{"submissionId": "s", "clientNonce": "n", "extra": 1}}}, MetaPath + ".submission.extra"},
		{"submission id missing", map[string]any{MetaKey: map[string]any{"version": 1, "submission": map[string]any{"clientNonce": "n"}}}, MetaPath + ".submission.submissionId"},
		{"client nonce missing", map[string]any{MetaKey: map[string]any{"version": 1, "submission": map[string]any{"submissionId": "s"}}}, MetaPath + ".submission.clientNonce"},
		{"submission id empty", map[string]any{MetaKey: map[string]any{"version": 1, "submission": map[string]any{"submissionId": "", "clientNonce": "n"}}}, MetaPath + ".submission.submissionId"},
		{"submission id not a string", map[string]any{MetaKey: map[string]any{"version": 1, "submission": map[string]any{"submissionId": 1, "clientNonce": "n"}}}, MetaPath + ".submission.submissionId"},
		{"submission id over bound", map[string]any{MetaKey: map[string]any{"version": 1, "submission": map[string]any{"submissionId": overBound(), "clientNonce": "n"}}}, MetaPath + ".submission.submissionId"},
		{"empty run id", map[string]any{MetaKey: map[string]any{"version": 1, "submission": map[string]any{"submissionId": "s", "clientNonce": "n", "runId": ""}}}, MetaPath + ".submission.runId"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			_, refusal := DecodePromptCorrelation(row.meta, Negotiated{Version: 1})
			require.NotNil(t, refusal)
			require.Equal(t, row.field, refusal.Field)
		})
	}
}

// TestDecodePromptCorrelationReadsAWireInteger proves a value that arrived through
// JSON decoding is the same integer an embedding Go host writes.
func TestDecodePromptCorrelationReadsAWireInteger(t *testing.T) {
	t.Parallel()

	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"acp-go.dev/lifecycle":{"version":1,`+
		`"submission":{"submissionId":"sub-1","clientNonce":"nonce-1"}}}`), &meta))

	submission, refusal := DecodePromptCorrelation(meta, Negotiated{Version: 1})
	require.Nil(t, refusal)
	require.Equal(t, "sub-1", submission.SubmissionID)

	decoder := json.NewDecoder(strings.NewReader("1"))
	decoder.UseNumber()

	var number json.Number

	require.NoError(t, decoder.Decode(&number))

	value, ok := integerValue(number)
	require.True(t, ok)
	require.Equal(t, 1, value)

	_, ok = integerValue(json.Number("x"))
	require.False(t, ok)
}

// TestIntegerValueReadsOnlyTheIntegersAnIntHolds proves the wire-integer read is
// exact rather than merely fractionless: a magnitude no int can hold names no
// version, whatever its whole-numberedness, while the integers a host actually
// writes — as a wire float, as a Go int, as a decoded number — read as themselves.
func TestIntegerValueReadsOnlyTheIntegersAnIntHolds(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		raw   any
		value int
	}{
		{"wire float", 1.0, 1},
		{"go int", 1, 1},
		{"negative", -7.0, -7},
		{"zero", 0.0, 0},
		{"largest exact float integer", float64(1 << 53), 1 << 53},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			value, ok := integerValue(row.raw)
			require.True(t, ok)
			require.Equal(t, row.value, value)
		})
	}

	for _, row := range []struct {
		name string
		raw  any
	}{
		{"fractional", 1.5},
		{"beyond every int", 1e300},
		{"below every int", -1e300},
		{"largest float", math.MaxFloat64},
		{"at the int64 ceiling", math.Ldexp(1, 63)},
		{"positive infinity", math.Inf(1)},
		{"not a number", math.NaN()},
		{"a string", "1"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			value, ok := integerValue(row.raw)
			require.False(t, ok)
			require.Zero(t, value)
		})
	}
}

// TestActionCorrelationValue proves the outbound correlation carries exactly the
// fixed members, with the optional ownership root present only when named.
func TestActionCorrelationValue(t *testing.T) {
	t.Parallel()

	bare := ActionCorrelation{
		StreamID: "stream-1",
		ActionID: "action-1",
		Owner:    Owner{Type: OwnerTurn, ID: "turn-1"},
	}
	require.Equal(t, map[string]any{
		"version":  1,
		"streamId": "stream-1",
		"action": map[string]any{
			"actionId": "action-1",
			"owner":    map[string]any{"type": "turn", "id": "turn-1"},
		},
	}, bare.Value())

	rooted := bare
	rooted.RunID = "run-1"

	action, ok := rooted.Value()["action"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "run-1", action["runId"])
}

func overBound() string {
	value := make([]byte, IdentifierBound+1)
	for index := range value {
		value[index] = 'x'
	}

	return string(value)
}

// TestPromptCorrelationMissingIsItsOwnVerdict proves the two verdicts a host
// reads from one field path stay distinct: a key the contract requires and the
// caller omitted is `missing`, and a key that is present where it may not be, or
// a member that is wrong, is `unsupported`.
func TestPromptCorrelationMissingIsItsOwnVerdict(t *testing.T) {
	t.Parallel()

	negotiated := Negotiated{Version: Version}

	_, refusal := DecodePromptCorrelation(map[string]any{}, negotiated)
	require.NotNil(t, refusal)
	require.True(t, refusal.Missing)
	require.Equal(t, MetaPath, refusal.Field)
	require.Equal(t, "missing "+MetaPath, refusal.Error())

	// The key present on a connection that negotiated nothing is the other
	// verdict on the same path.
	_, refusal = DecodePromptCorrelation(map[string]any{MetaKey: map[string]any{}}, Negotiated{})
	require.NotNil(t, refusal)
	require.False(t, refusal.Missing)
	require.Equal(t, "unsupported "+MetaPath, refusal.Error())
}
