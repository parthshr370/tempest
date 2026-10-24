// Package question carries non-sensitive user questions across host transports.
package question

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Unavailable tells the model how to continue without a human client.
var Unavailable = errors.New("ask has no interactive user client; proceed with your best judgment and state any assumptions")

// Option is a labeled choice with optional explanatory text.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question asks for one or more choices, or optional free text.
type Question struct {
	ID        string   `json:"id"`
	Question  string   `json:"question"`
	Options   []Option `json:"options"`
	Multi     bool     `json:"multi,omitempty"`
	AllowText bool     `json:"allow_text,omitempty"`
}

// Request binds questions to the originating durable tool call.
type Request struct {
	SessionID  string     `json:"sessionId"`
	ToolCallID string     `json:"toolCallId"`
	Questions  []Question `json:"questions"`
}

// Answer retains labels rather than client-specific option indexes.
type Answer struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Text     string   `json:"text,omitempty"`
}

// Handler must stop waiting when its context is cancelled.
type Handler func(context.Context, Request) ([]Answer, error)

// Validate rejects ambiguous questions before presenting any UI.
func Validate(questions []Question) error {
	if len(questions) == 0 || len(questions) > 20 {
		return errors.New("ask requires between 1 and 20 questions")
	}
	ids := map[string]bool{}
	for _, q := range questions {
		if strings.TrimSpace(q.ID) == "" || strings.TrimSpace(q.Question) == "" || ids[q.ID] {
			return errors.New("ask requires unique nonempty question ids and question text")
		}
		ids[q.ID] = true
		if len(q.Options) == 0 && !q.AllowText {
			return fmt.Errorf("question %s needs options or allow_text", q.ID)
		}
		labels := map[string]bool{}
		for _, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" || labels[o.Label] {
				return errors.New("ask option labels must be nonempty and unique")
			}
			labels[o.Label] = true
		}
	}
	return nil
}

// ValidateAnswers distrusts client responses just like model arguments.
func ValidateAnswers(questions []Question, answers []Answer) error {
	if len(questions) != len(answers) {
		return errors.New("ask client returned the wrong number of answers")
	}
	for i, q := range questions {
		a := answers[i]
		if a.ID != q.ID || (!q.Multi && len(a.Selected) > 1) || (!q.AllowText && a.Text != "") {
			return fmt.Errorf("invalid answer for question %s", q.ID)
		}
		labels := map[string]bool{}
		for _, o := range q.Options {
			labels[o.Label] = true
		}
		seen := map[string]bool{}
		for _, label := range a.Selected {
			if !labels[label] || seen[label] {
				return fmt.Errorf("invalid selection for question %s", q.ID)
			}
			seen[label] = true
		}
		if (!q.Multi || len(q.Options) == 0) && len(a.Selected) == 0 && strings.TrimSpace(a.Text) == "" {
			return fmt.Errorf("question %s was not answered", q.ID)
		}
	}
	return nil
}

// FormSchema is the restricted JSON Schema used by elicitation.
type FormSchema struct {
	Type       string                `json:"type,omitempty"`
	Title      string                `json:"title,omitempty"`
	Properties map[string]FormSchema `json:"properties,omitempty"`
	Required   []string              `json:"required,omitempty"`
	Enum       []string              `json:"enum,omitempty"`
	EnumNames  []string              `json:"enumNames,omitempty"`
	OneOf      []FormSchema          `json:"oneOf,omitempty"`
	Const      string                `json:"const,omitempty"`
	Items      *FormSchema           `json:"items,omitempty"`
}

// Form builds the flat restricted schema understood by ACP and MCP elicitation.
func Form(q Question) FormSchema {
	properties := map[string]FormSchema{}
	required := []string{}
	if len(q.Options) > 0 {
		labels := make([]string, 0, len(q.Options))
		choices := make([]FormSchema, 0, len(q.Options))
		for _, option := range q.Options {
			labels = append(labels, option.Label)
			name := option.Label
			if option.Description != "" {
				name += " - " + option.Description
			}
			choices = append(choices, FormSchema{Const: option.Label, Title: name})
		}
		if q.Multi {
			properties["selected"] = FormSchema{Type: "array", Title: q.Question, Items: &FormSchema{Type: "string", Enum: labels}}
		} else {
			properties["selected"] = FormSchema{Type: "string", Title: q.Question, OneOf: choices}
		}
		if !q.AllowText {
			required = append(required, "selected")
		}
	}
	if q.AllowText {
		properties["text"] = FormSchema{Type: "string", Title: "Other answer or additional context"}
		if len(q.Options) == 0 {
			required = append(required, "text")
		}
	}
	return FormSchema{Type: "object", Properties: properties, Required: required}
}

// DecodeForm converts an elicitation response into a validated answer.
func DecodeForm(q Question, raw json.RawMessage) (Answer, error) {
	var response struct {
		Action  string `json:"action"`
		Content struct {
			Selected json.RawMessage `json:"selected"`
			Text     string          `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return Answer{}, err
	}
	if response.Action != "accept" {
		return Answer{}, errors.New("user declined or cancelled ask; proceed with your best judgment")
	}
	a := Answer{ID: q.ID, Selected: []string{}, Text: response.Content.Text}
	if len(response.Content.Selected) > 0 && string(response.Content.Selected) != "null" {
		if q.Multi {
			if err := json.Unmarshal(response.Content.Selected, &a.Selected); err != nil {
				return a, err
			}
		} else {
			var label string
			if err := json.Unmarshal(response.Content.Selected, &label); err != nil {
				return a, err
			}
			a.Selected = []string{label}
		}
	}
	return a, ValidateAnswers([]Question{q}, []Answer{a})
}
