package prompt

import (
	_ "embed"
	"strings"
	"text/template"
)

// PromptTool describes one tool active in the current session.
type PromptTool struct {
	Name  string
	Label string
}

// PromptSkill describes a skill that can be loaded by the current session.
type PromptSkill struct {
	Name        string
	Description string
	Location    string
}

// PromptRule describes an optional domain rule projected into the system prompt.
type PromptRule struct {
	Name        string
	Description string
	Globs       []string
}

// PromptData is the typed data model rendered by the embedded system prompt template.
type PromptData struct {
	ProductName         string
	CustomPrompt        string
	HasCustomPrompt     bool
	CapabilitiesSection string
	AppendSystemPrompt  string
	Cwd                 string
	Date                string
	ContextFiles        []ContextFile
	HasContextFiles     bool
	Tools               []PromptTool
	Skills              []PromptSkill
	HasSkills           bool
	Rules               []PromptRule
	HasRules            bool
	GenericRules        []string
	HasGenericRules     bool
	Guidelines          []string
	Personality         string
	HasRead             bool
	HasBash             bool
	HasEdit             bool
	HasHashline         bool
	HasWrite            bool
	HasGrep             bool
	HasFind             bool
	HasLs               bool
	HasTodoWrite        bool
	HasTask             bool
	HasSkill            bool
	HasWebSearch        bool
	HasWebFetch         bool
	HasAttachment       bool
}

//go:embed templates/system.md.tmpl
var systemTemplateText string

var xmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

var systemTemplate = template.Must(template.New("system.md.tmpl").Funcs(template.FuncMap{
	"xml": escapeXML,
}).Parse(systemTemplateText))

// renderSystemPrompt fails fast because a broken embedded template is a build-time programming error.
func renderSystemPrompt(data PromptData) string {
	var output strings.Builder
	if err := systemTemplate.Execute(&output, data); err != nil {
		panic("render embedded system prompt: " + err.Error())
	}
	return output.String()
}

func escapeXML(value string) string {
	return xmlEscaper.Replace(value)
}
