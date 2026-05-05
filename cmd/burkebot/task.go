package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Task is a server-defined unit of work the API exposes. Each task pins
// the prompt template, the JSON Schema the model output must conform to,
// and the runtime constraints — all of which the caller cannot override.
// The caller supplies only the values for the inputs the task declares.
type Task struct {
	// Name is the slug used in the URL: POST /api/tasks/<name>/runs.
	Name string `json:"name"`

	// Project must match a Project.Name in projects.json. The audit
	// bundle and repo dir come from that project.
	Project string `json:"project"`

	// PromptTemplatePath is read at server startup and parsed as a Go
	// text/template. The data passed to the template is a map[string]any
	// with one entry per declared Input — value is the absolute path to
	// the file the input was written to (so the prompt can tell the model
	// "read agenda from {{.Agenda}}").
	PromptTemplatePath string `json:"prompt_template"`

	// OutputSchemaPath is the JSON Schema (draft 2020-12) the runner
	// passes to `codex --output-schema`. The server also re-validates
	// the output against it before returning to the caller.
	OutputSchemaPath string `json:"output_schema"`

	// Inputs are the request-body fields the caller must supply. Missing
	// inputs cause a 400; extra inputs are rejected (no silent drop).
	Inputs []TaskInput `json:"inputs"`

	// TimeoutSeconds caps the run duration. 0 means no override (the
	// runner / systemd-run defaults apply).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// promptTmpl and outputSchema are populated by loadTasks; not
	// serialized.
	promptTmpl   *template.Template
	outputSchema *jsonschema.Schema
}

// TaskInput describes one field the API caller must supply for a task.
// The runner writes the value to a file in the per-run scratch directory;
// the prompt template references that file by path.
type TaskInput struct {
	// Name is the JSON key the caller uses in the request body.
	Name string `json:"name"`

	// Filename is the basename written into the scratch directory. The
	// absolute path becomes available in the prompt template under the
	// camelcased input name (e.g. input "agenda" → {{.Agenda}}).
	Filename string `json:"filename"`

	// MaxBytes caps the size of this input. The server rejects oversize
	// values with 413 before writing anything to disk. 0 means "no cap"
	// — set it to a real value for any input that comes from a third
	// party.
	MaxBytes int `json:"max_bytes,omitempty"`
}

// loadTasks reads tasks.json from disk and validates each entry: the
// prompt template parses, the schema file exists and parses, and input
// names are unique within the task. Returns nil if path is empty or the
// file does not exist (lets the dashboard run without the API enabled).
func loadTasks(path string) ([]Task, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tasks []Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for i := range tasks {
		t := &tasks[i]
		if t.Name == "" {
			return nil, fmt.Errorf("task %d has no name", i)
		}
		if t.Project == "" {
			return nil, fmt.Errorf("task %q has no project", t.Name)
		}
		if t.PromptTemplatePath == "" {
			return nil, fmt.Errorf("task %q has no prompt_template", t.Name)
		}
		if t.OutputSchemaPath == "" {
			return nil, fmt.Errorf("task %q has no output_schema", t.Name)
		}

		seen := make(map[string]struct{}, len(t.Inputs))
		for _, in := range t.Inputs {
			if in.Name == "" || in.Filename == "" {
				return nil, fmt.Errorf("task %q: every input needs name and filename", t.Name)
			}
			if _, dup := seen[in.Name]; dup {
				return nil, fmt.Errorf("task %q: duplicate input name %q", t.Name, in.Name)
			}
			seen[in.Name] = struct{}{}
		}

		tmplBytes, err := os.ReadFile(t.PromptTemplatePath)
		if err != nil {
			return nil, fmt.Errorf("task %q: reading prompt template: %w", t.Name, err)
		}
		tmpl, err := template.New(t.Name).Parse(string(tmplBytes))
		if err != nil {
			return nil, fmt.Errorf("task %q: parsing prompt template: %w", t.Name, err)
		}
		t.promptTmpl = tmpl

		schemaBytes, err := os.ReadFile(t.OutputSchemaPath)
		if err != nil {
			return nil, fmt.Errorf("task %q: reading output schema: %w", t.Name, err)
		}
		schema, err := compileJSONSchema(t.OutputSchemaPath, schemaBytes)
		if err != nil {
			return nil, fmt.Errorf("task %q: compiling output schema: %w", t.Name, err)
		}
		t.outputSchema = schema
	}
	return tasks, nil
}

// compileJSONSchema parses raw JSON Schema bytes via the
// santhosh-tekuri/jsonschema compiler. The path is used as the schema's
// resource ID so error messages have something to point at.
func compileJSONSchema(path string, data []byte) (*jsonschema.Schema, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(path, raw); err != nil {
		return nil, err
	}
	return c.Compile(path)
}

// renderPrompt builds the prompt text by executing the task's template
// with the per-input file paths. Keys in inputPaths are the input names
// (lowercase as declared); the template sees them as TitleCased fields
// because Go's text/template requires exported field names on struct-like
// access — using a map sidesteps that. The template should reference
// inputs as {{index . "agenda"}} or pass via custom dot data.
//
// To keep templates readable we expose inputs both as a map (`{{.Inputs.agenda}}`)
// and at the top level (`{{.agenda}}`).
func (t *Task) renderPrompt(inputPaths map[string]string) (string, error) {
	data := make(map[string]any, len(inputPaths)+1)
	for k, v := range inputPaths {
		data[k] = v
	}
	data["Inputs"] = inputPaths
	var buf bytes.Buffer
	if err := t.promptTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing prompt template: %w", err)
	}
	return strings.TrimRight(buf.String(), "\n") + "\n", nil
}

// validateOutput parses raw as JSON and checks it against the task's
// output schema. Returns the parsed value (suitable for re-marshaling
// into the API response) on success.
func (t *Task) validateOutput(raw []byte) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("output is not valid JSON: %w", err)
	}
	if err := t.outputSchema.Validate(v); err != nil {
		return nil, fmt.Errorf("output does not match schema: %w", err)
	}
	return v, nil
}
