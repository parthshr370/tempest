package question

import (
	"encoding/json"
	"errors"
	"strconv"
)

// LegacyForm uses boolean fields because MCP 2025-06-18 doesn't support array enums.
func LegacyForm(prompt Question) FormSchema {
	if !prompt.Multi || len(prompt.Options) == 0 {
		form := Form(prompt)
		if selected, ok := form.Properties["selected"]; ok {
			for _, choice := range selected.OneOf {
				selected.Enum = append(selected.Enum, choice.Const)
				selected.EnumNames = append(selected.EnumNames, choice.Title)
			}
			selected.OneOf = nil
			form.Properties["selected"] = selected
		}
		return form
	}
	properties := make(map[string]FormSchema, len(prompt.Options)+1)
	for index, option := range prompt.Options {
		title := option.Label
		if option.Description != "" {
			title += " - " + option.Description
		}
		properties["choice_"+strconv.Itoa(index)] = FormSchema{Type: "boolean", Title: title}
	}
	if prompt.AllowText {
		properties["text"] = FormSchema{Type: "string", Title: "Other answer or additional context"}
	}
	return FormSchema{Type: "object", Properties: properties}
}

// DecodeLegacyForm converts the older primitive-only form into the same answer contract.
func DecodeLegacyForm(prompt Question, raw json.RawMessage) (Answer, error) {
	if !prompt.Multi || len(prompt.Options) == 0 {
		return DecodeForm(prompt, raw)
	}
	var response struct {
		Action  string                     `json:"action"`
		Content map[string]json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return Answer{}, err
	}
	if response.Action != "accept" {
		return Answer{}, errors.New("user declined or cancelled ask; proceed with your best judgment")
	}
	answer := Answer{ID: prompt.ID, Selected: []string{}}
	for index, option := range prompt.Options {
		if encoded, ok := response.Content["choice_"+strconv.Itoa(index)]; ok {
			var selected bool
			if err := json.Unmarshal(encoded, &selected); err != nil {
				return Answer{}, err
			}
			if selected {
				answer.Selected = append(answer.Selected, option.Label)
			}
		}
	}
	if encoded, ok := response.Content["text"]; ok {
		if err := json.Unmarshal(encoded, &answer.Text); err != nil {
			return Answer{}, err
		}
	}
	return answer, ValidateAnswers([]Question{prompt}, []Answer{answer})
}
