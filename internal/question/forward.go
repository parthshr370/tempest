package question

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Elicit is a user request that did not come from the ask tool: an MCP
// server's elicitation/create, forwarded unchanged, or an authorization URL
// Tempest must show. Form mode carries the server's restricted JSON Schema;
// URL mode carries a URL the user opens out of band.
type Elicit struct {
	SessionID string
	// Server names the MCP server asking; every surface shows it.
	Server  string
	Mode    string
	Message string
	Schema  json.RawMessage
	URL     string
	// ElicitationID identifies a URL-mode request to its server.
	ElicitationID string
}

// ElicitResult is the user's reply in the MCP accept/decline/cancel model.
// Content is the submitted form object and only accompanies a form accept.
type ElicitResult struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content,omitempty"`
}

// Elicitor presents an Elicit and returns once the user answers or ctx ends.
// Unavailable means no interactive user can be reached.
type Elicitor func(context.Context, Elicit) (ElicitResult, error)

// ForwardParams is the elicitation/create request an ACP or MCP client gets.
type ForwardParams struct {
	SessionID       string          `json:"sessionId,omitempty"`
	Mode            string          `json:"mode,omitempty"`
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema,omitempty"`
	URL             string          `json:"url,omitempty"`
	ElicitationID   string          `json:"elicitationId,omitempty"`
}

// Params builds the client request. The message names the server so the
// user knows who asks; omitMode drops the mode field for 2025-06-18 peers,
// which only know form mode.
func (e Elicit) Params(omitMode bool) ForwardParams {
	params := ForwardParams{SessionID: e.SessionID, Mode: e.Mode, Message: e.Display()}
	if omitMode {
		params.Mode = ""
	}
	if e.Mode == "url" {
		params.URL, params.ElicitationID = e.URL, e.ElicitationID
	} else {
		params.RequestedSchema = e.Schema
	}
	return params
}

// Display is the message prefixed with the asking server.
func (e Elicit) Display() string {
	if e.Server == "" {
		return e.Message
	}
	return "MCP server " + e.Server + ": " + e.Message
}

// Validate rejects a request no surface can show safely: an unknown mode, a
// URL that is not absolute http(s), or a form schema outside the flat
// primitive subset the MCP specification allows.
func (e Elicit) Validate() error {
	switch e.Mode {
	case "url":
		u, err := url.Parse(e.URL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || e.ElicitationID == "" {
			return errors.New("URL elicitation needs an absolute http(s) url and an elicitationId")
		}
		return nil
	case "form":
		_, err := parseForm(e.Schema)
		return err
	default:
		return fmt.Errorf("unknown elicitation mode %q", e.Mode)
	}
}

// DecodeResult validates a client's reply: a known action and, for an
// accepted form, an object that satisfies the requested schema.
func DecodeResult(e Elicit, raw json.RawMessage) (ElicitResult, error) {
	var result ElicitResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return ElicitResult{}, fmt.Errorf("invalid elicitation reply: %w", err)
	}
	switch result.Action {
	case "decline", "cancel":
		return ElicitResult{Action: result.Action}, nil
	case "accept":
	default:
		return ElicitResult{}, fmt.Errorf("invalid elicitation action %q", result.Action)
	}
	if e.Mode == "url" {
		return ElicitResult{Action: "accept"}, nil
	}
	form, err := parseForm(e.Schema)
	if err != nil {
		return ElicitResult{}, err
	}
	if err := form.check(result.Content); err != nil {
		return ElicitResult{}, err
	}
	return result, nil
}

// formField is one primitive property of a restricted form schema.
type formField struct {
	Name        string
	Type        string
	Title       string
	Description string
	Choices     []string
	Labels      []string
	Multi       bool
	Default     json.RawMessage
	Required    bool
	Minimum     *float64
	Maximum     *float64
}

type form struct{ fields []formField }

type fieldSchema struct {
	Type        string          `json:"type"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Enum        []string        `json:"enum"`
	EnumNames   []string        `json:"enumNames"`
	OneOf       []constChoice   `json:"oneOf"`
	AnyOf       []constChoice   `json:"anyOf"`
	Items       *fieldSchema    `json:"items"`
	Default     json.RawMessage `json:"default"`
	Minimum     *float64        `json:"minimum"`
	Maximum     *float64        `json:"maximum"`
}

type constChoice struct {
	Const string `json:"const"`
	Title string `json:"title"`
}

// parseForm reads a requestedSchema in property order. Nested objects and
// arrays other than enum multi-selects are refused, as the specification
// limits forms to flat primitive properties.
func parseForm(raw json.RawMessage) (form, error) {
	var top struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &top) != nil || top.Type != "object" {
		return form{}, errors.New("form elicitation needs an object requestedSchema")
	}
	names, err := objectKeys(top.Properties)
	if err != nil {
		return form{}, errors.New("requestedSchema properties must be an object")
	}
	var properties map[string]fieldSchema
	if err := json.Unmarshal(top.Properties, &properties); err != nil {
		return form{}, fmt.Errorf("requestedSchema properties: %w", err)
	}
	var out form
	for _, name := range names {
		schema := properties[name]
		field := formField{Name: name, Type: schema.Type, Title: schema.Title, Description: schema.Description, Default: schema.Default, Required: slices.Contains(top.Required, name), Minimum: schema.Minimum, Maximum: schema.Maximum}
		switch schema.Type {
		case "string":
			field.Choices, field.Labels = choices(schema.Enum, schema.EnumNames, schema.OneOf)
		case "number", "integer", "boolean":
		case "array":
			if schema.Items == nil {
				return form{}, fmt.Errorf("form field %s: arrays must be enum multi-selects", name)
			}
			field.Multi = true
			field.Choices, field.Labels = choices(schema.Items.Enum, schema.Items.EnumNames, schema.Items.AnyOf)
			if len(field.Choices) == 0 {
				return form{}, fmt.Errorf("form field %s: arrays must be enum multi-selects", name)
			}
		default:
			return form{}, fmt.Errorf("form field %s has unsupported type %q", name, schema.Type)
		}
		out.fields = append(out.fields, field)
	}
	for _, name := range top.Required {
		if !slices.Contains(names, name) {
			return form{}, fmt.Errorf("required form field %s is not a property", name)
		}
	}
	return out, nil
}

func choices(enum, names []string, titled []constChoice) ([]string, []string) {
	if len(titled) > 0 {
		values := make([]string, 0, len(titled))
		labels := make([]string, 0, len(titled))
		for _, choice := range titled {
			values = append(values, choice.Const)
			labels = append(labels, cmpOr(choice.Title, choice.Const))
		}
		return values, labels
	}
	labels := make([]string, len(enum))
	for i, value := range enum {
		labels[i] = value
		if i < len(names) && names[i] != "" {
			labels[i] = names[i]
		}
	}
	return enum, labels
}

func cmpOr(first, second string) string {
	if first != "" {
		return first
	}
	return second
}

// objectKeys returns a JSON object's keys in document order.
func objectKeys(raw json.RawMessage) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := token.(string)
		keys = append(keys, key)
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// check validates submitted content against the form's fields.
func (f form) check(content json.RawMessage) error {
	var values map[string]json.RawMessage
	if len(content) == 0 || json.Unmarshal(content, &values) != nil || values == nil {
		return errors.New("accepted form elicitation must carry an object content")
	}
	for name := range values {
		if !slices.ContainsFunc(f.fields, func(field formField) bool { return field.Name == name }) {
			return fmt.Errorf("form content has unknown field %s", name)
		}
	}
	for _, field := range f.fields {
		raw, present := values[field.Name]
		if !present {
			if field.Required {
				return fmt.Errorf("form field %s is required", field.Name)
			}
			continue
		}
		if err := field.check(raw); err != nil {
			return err
		}
	}
	return nil
}

func (field formField) check(raw json.RawMessage) error {
	invalid := fmt.Errorf("form field %s must be %s", field.Name, field.describe())
	switch field.Type {
	case "string":
		var value string
		if json.Unmarshal(raw, &value) != nil || (len(field.Choices) > 0 && !slices.Contains(field.Choices, value)) {
			return invalid
		}
	case "number", "integer":
		var value float64
		if json.Unmarshal(raw, &value) != nil || (field.Type == "integer" && value != math.Trunc(value)) ||
			(field.Minimum != nil && value < *field.Minimum) || (field.Maximum != nil && value > *field.Maximum) {
			return invalid
		}
	case "boolean":
		var value bool
		if json.Unmarshal(raw, &value) != nil {
			return invalid
		}
	case "array":
		var values []string
		if json.Unmarshal(raw, &values) != nil {
			return invalid
		}
		for _, value := range values {
			if !slices.Contains(field.Choices, value) {
				return invalid
			}
		}
	}
	return nil
}

func (field formField) describe() string {
	switch {
	case field.Multi:
		return "a list of: " + strings.Join(field.Choices, ", ")
	case len(field.Choices) > 0:
		return "one of: " + strings.Join(field.Choices, ", ")
	case field.Type == "integer":
		return "an integer" + field.bounds()
	case field.Type == "number":
		return "a number" + field.bounds()
	case field.Type == "boolean":
		return "true or false"
	}
	return "text"
}

func (field formField) bounds() string {
	switch {
	case field.Minimum != nil && field.Maximum != nil:
		return fmt.Sprintf(" from %g to %g", *field.Minimum, *field.Maximum)
	case field.Minimum != nil:
		return fmt.Sprintf(" of at least %g", *field.Minimum)
	case field.Maximum != nil:
		return fmt.Sprintf(" of at most %g", *field.Maximum)
	}
	return ""
}

// prompt renders one field for a line-based prompt.
func (field formField) prompt() string {
	var text strings.Builder
	text.WriteString("\n" + cmpOr(field.Title, field.Name))
	if field.Required {
		text.WriteString(" (required)")
	}
	if field.Description != "" {
		text.WriteString(": " + field.Description)
	}
	text.WriteString("\n")
	for i, label := range field.Labels {
		fmt.Fprintf(&text, "  %d. %s\n", i+1, label)
	}
	hint := "Enter " + field.describe()
	if field.Multi {
		hint = "Enter numbers separated by commas"
	} else if len(field.Choices) > 0 {
		hint = "Enter a number"
	}
	if len(field.Default) > 0 {
		hint += ", or press Enter for " + string(field.Default)
	} else if !field.Required {
		hint += ", or press Enter to skip"
	}
	return text.String() + hint + ": "
}

// answer converts one typed line into the field's JSON value. A nil value
// with a nil error skips an optional field.
func (field formField) answer(line string) (json.RawMessage, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		switch {
		case len(field.Default) > 0:
			return field.Default, field.check(field.Default)
		case field.Required:
			return nil, errors.New("a value is required")
		default:
			return nil, nil
		}
	}
	var encoded json.RawMessage
	var err error
	switch field.Type {
	case "string":
		text := line
		if len(field.Choices) > 0 {
			if text, err = field.pick(line); err != nil {
				return nil, err
			}
		}
		encoded, err = json.Marshal(text)
	case "number", "integer":
		number, parseErr := strconv.ParseFloat(line, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("enter %s", field.describe())
		}
		encoded, err = json.Marshal(number)
	case "boolean":
		switch strings.ToLower(line) {
		case "y", "yes", "true":
			encoded = json.RawMessage("true")
		case "n", "no", "false":
			encoded = json.RawMessage("false")
		default:
			return nil, errors.New("enter yes or no")
		}
	case "array":
		var picked []string
		for _, part := range strings.Split(line, ",") {
			choice, pickErr := field.pick(strings.TrimSpace(part))
			if pickErr != nil {
				return nil, pickErr
			}
			picked = append(picked, choice)
		}
		encoded, err = json.Marshal(picked)
	}
	if err != nil {
		return nil, err
	}
	return encoded, field.check(encoded)
}

// pick resolves an offered choice by its number, value or label.
func (field formField) pick(text string) (string, error) {
	if index, err := strconv.Atoi(text); err == nil && index >= 1 && index <= len(field.Choices) {
		return field.Choices[index-1], nil
	}
	for i, choice := range field.Choices {
		if text == choice || text == field.Labels[i] {
			return choice, nil
		}
	}
	return "", errors.New("enter an offered option number")
}
