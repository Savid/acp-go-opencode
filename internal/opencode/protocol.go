//nolint:tagliatelle // Native HTTP and sync JSON use ID suffixes and aggregate_id.
package opencode

import "encoding/json"

type NativeSession struct {
	ParentID  string         `json:"parentID"`
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Directory string         `json:"directory"`
	Agent     string         `json:"agent"`
	Metadata  map[string]any `json:"metadata"`
	Model     struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Time struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

type NativeMessage struct {
	Info  NativeMessageInfo `json:"info"`
	Parts []NativePart      `json:"parts"`
}

type NativeMessageInfo struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"sessionID"`
	Role       string          `json:"role"`
	ParentID   string          `json:"parentID"`
	ModelID    string          `json:"modelID"`
	ProviderID string          `json:"providerID"`
	Finish     string          `json:"finish"`
	Tokens     NativeTokens    `json:"tokens"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Error      *NativeError    `json:"error,omitempty"`
	Time       struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

type NativeError struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Message string `json:"message"`
	Data    struct {
		Message string `json:"message"`
	} `json:"data"`
}

type NativePart struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionID"`
	MessageID string          `json:"messageID"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	CallID    string          `json:"callID"`
	Tool      string          `json:"tool"`
	State     json.RawMessage `json:"state"`
	Mime      string          `json:"mime"`
	Filename  string          `json:"filename"`
	URL       string          `json:"url"`
	Raw       json.RawMessage `json:"-"`
}

// NativeAttachment is a native file part carried inside a completed tool
// state's attachments array. The URL is a data URL, a file URL/path, or a
// remote location.
type NativeAttachment struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Mime     string `json:"mime"`
	Filename string `json:"filename"`
	URL      string `json:"url"`
}

func (p *NativePart) UnmarshalJSON(data []byte) error {
	type alias NativePart

	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	*p = NativePart(value)
	p.Raw = append(p.Raw[:0], data...)

	return nil
}

type NativeTokens struct {
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	Reasoning float64 `json:"reasoning"`
	Cache     struct {
		Read  float64 `json:"read"`
		Write float64 `json:"write"`
	} `json:"cache"`
}

type NativeTodo struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

type NativeSessionStatus struct {
	Type string `json:"type"`
}

type NativeAgent struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

type NativeCommand struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Hints       []string `json:"hints"`
}

type Event struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
	Raw        json.RawMessage `json:"-"`
}

func (e *Event) UnmarshalJSON(data []byte) error {
	type alias Event

	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	*e = Event(value)
	e.Raw = append(e.Raw[:0], data...)

	return nil
}

type PermissionTool struct {
	MessageID string `json:"messageID"`
	CallID    string `json:"callID"`
}

type QuestionInfo struct {
	Question string           `json:"question"`
	Options  []QuestionOption `json:"options"`
	Multiple bool             `json:"multiple"`
	Custom   *bool            `json:"custom,omitempty"`
}

type QuestionOption struct {
	Label string `json:"label"`
}

type QuestionTool struct {
	MessageID string `json:"messageID"`
	CallID    string `json:"callID"`
}

type MessageRequest struct {
	MessageID string           `json:"messageID,omitempty"`
	Model     *ModelSelector   `json:"model,omitempty"`
	Agent     string           `json:"agent,omitempty"`
	Variant   string           `json:"variant,omitempty"`
	Parts     []map[string]any `json:"parts"`
	Format    *OutputFormat    `json:"format,omitempty"`
}

const OutputFormatJSONSchema = "json_schema"

type OutputFormat struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema"`
}

type CommandRequest struct {
	MessageID string           `json:"messageID,omitempty"`
	Agent     string           `json:"agent,omitempty"`
	Model     string           `json:"model,omitempty"`
	Variant   string           `json:"variant,omitempty"`
	Command   string           `json:"command"`
	Arguments string           `json:"arguments"`
	Parts     []map[string]any `json:"parts,omitempty"`
}

type ModelSelector struct {
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
}

type ProviderInfo struct {
	ID     string                   `json:"id"`
	Name   string                   `json:"name"`
	Models map[string]ProviderModel `json:"models"`
}

type ProviderModel struct {
	ID           string                     `json:"id"`
	Name         string                     `json:"name"`
	Limit        map[string]any             `json:"limit"`
	Capabilities *ProviderModelCapabilities `json:"capabilities"`
	// Variants are the model's named request presets, keyed by the name a
	// prompt selects one with. On a reasoning model the catalog names them by
	// effort level, so the keys are the effort levels the model supports.
	Variants map[string]map[string]any `json:"variants"`
}

// ProviderModelCapabilities is the authenticated provider catalog's exhaustive
// per-model capability record.
type ProviderModelCapabilities struct {
	Input ProviderModelInputCapabilities `json:"input"`
}

// ProviderModelInputCapabilities reports which input kinds the model accepts.
// A nil field means the catalog did not state the fact either way.
type ProviderModelInputCapabilities struct {
	Image *bool `json:"image"`
}

type Catalog struct {
	Providers []ProviderInfo    `json:"providers"`
	Default   map[string]string `json:"default"`
}
type PermissionRequest struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"sessionID"`
	Permission string         `json:"permission"`
	Patterns   []string       `json:"patterns"`
	Tool       PermissionTool `json:"tool"`
}
type QuestionRequest struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionID"`
	Questions []QuestionInfo `json:"questions"`
	Tool      QuestionTool   `json:"tool"`
}
type SyncEvent struct {
	Raw         json.RawMessage            `json:"-"`
	ID          string                     `json:"id"`
	AggregateID string                     `json:"aggregate_id"`
	Sequence    int64                      `json:"seq"`
	Type        string                     `json:"type"`
	Data        map[string]json.RawMessage `json:"data"`
}

func (e *SyncEvent) UnmarshalJSON(data []byte) error {
	type alias SyncEvent

	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	*e = SyncEvent(value)
	e.Raw = append(e.Raw[:0], data...)

	return nil
}
func (e SyncEvent) MarshalJSON() ([]byte, error) {
	if e.Raw != nil {
		return e.Raw, nil
	}

	type alias SyncEvent

	return json.Marshal(alias(e))
}

// AllowsCustom applies the native question default.
func (q QuestionInfo) AllowsCustom() bool { return q.Custom == nil || *q.Custom }
