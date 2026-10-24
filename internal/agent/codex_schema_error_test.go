package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

const codexRejectedArraySchema = `{"error":{"message":"Invalid json schema: field ` + "`type`: unknown variant `array`, expected one of `string`, `number`, `integer`, `boolean`, `null`" + `","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`

func TestCodexAgent_ProviderSchemaRejection(t *testing.T) {
	for _, carrier := range []string{"error", "turn.failed", "stderr"} {
		t.Run(carrier, func(t *testing.T) {
			dir := t.TempDir()
			message := "Unexpected status code 400 Bad Request: " + codexRejectedArraySchema
			var event []byte
			var err error
			switch carrier {
			case "error":
				event, err = json.Marshal(map[string]any{"type": "error", "message": message})
			case "turn.failed":
				event, err = json.Marshal(map[string]any{"type": "turn.failed", "error": map[string]string{"message": message}})
			case "stderr":
				event = []byte(codexRejectedArraySchema)
			}
			if err != nil {
				t.Fatal(err)
			}
			redirect := ""
			if carrier == "stderr" {
				redirect = " >&2"
			}
			bin := writeFakeCodex(t, dir, "#!/bin/sh\nprintf '%s\\n' \"$CODEX_FAILURE\""+redirect+"\nexit 1\n",
				"@echo off\r\necho %CODEX_FAILURE%"+redirect+"\r\nexit /b 1\r\n")
			attempts := 0
			result, err := (&codexAgent{bin: bin, extraArgs: []string{"-p", "deepseek"}}).Run(context.Background(), RunOpts{
				Prompt:     "review",
				CWD:        dir,
				JSONSchema: json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object"}}},"required":["findings"]}`),
				Env:        []string{"CODEX_FAILURE=" + string(event)},
				OnAttempt:  func(Attempt) { attempts++ },
			})
			if err == nil || result != nil {
				t.Fatalf("Run = (%+v, %v), want schema incompatibility failure", result, err)
			}
			for _, want := range []string{"codex structured-output incompatibility", "provider does not accept", "--output-schema", "profile", "agent", "Invalid json schema:"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want %q", err, want)
				}
			}
			if !IsTerminalProviderError(err) || IsStructuredOutputRejected(err) {
				t.Errorf("provider rejection must be terminal, not a retryable analyzer response: %v", err)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Errorf("error must preserve subprocess exit: %v", err)
			}
			if attempts != 1 {
				t.Errorf("attempts = %d, want 1", attempts)
			}
		})
	}
}

func TestCodexRejectsOutputSchema(t *testing.T) {
	for _, tc := range []struct {
		name   string
		detail string
		want   bool
	}{
		{"reported provider envelope", codexRejectedArraySchema, true},
		{"HTTP prefix and suffix", "Unexpected status code 400: " + codexRejectedArraySchema + ", url: https://example.invalid/responses", true},
		{"schema code", `{"error":{"message":"Unsupported constraint","code":"invalid_json_schema"}}`, true},
		{"Responses format parameter", `{"error":{"message":"Unsupported constraint","type":"invalid_request_error","param":"text.format.schema"}}`, true},
		{"chat format parameter", `{"error":{"message":"Unsupported constraint","code":"invalid_request_error","param":"response_format.json_schema.schema"}}`, true},
		{"message only", "Invalid json schema: unsupported array", true},
		{"authentication", `{"error":{"message":"Invalid API key","type":"authentication_error"}}`, false},
		{"unrelated invalid request", `{"error":{"message":"Unknown model","type":"invalid_request_error","param":"model"}}`, false},
		{"tool schema", `{"error":{"message":"Unsupported constraint","type":"invalid_request_error","param":"tools.0.parameters"}}`, false},
		{"local schema file", "Error reading output schema file: no such file or directory", false},
		{"quoted schema message", "A tool printed Invalid json schema: in its output", false},
		{"ordinary output", `{"summary":"Invalid json schema: example","findings":[]}`, false},
		{"malformed envelope", `{"error":{"message":"Invalid json schema:`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexRejectsOutputSchema(tc.detail); got != tc.want {
				t.Errorf("codexRejectsOutputSchema(%q) = %t, want %t", tc.detail, got, tc.want)
			}
		})
	}
}

func TestCodexAgent_SchemaRejectionRequiresOutputSchema(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCodex(t, dir, "#!/bin/sh\nprintf '%s\\n' \"$CODEX_FAILURE\" >&2\nexit 1\n",
		"@echo off\r\necho %CODEX_FAILURE% >&2\r\nexit /b 1\r\n")
	_, err := (&codexAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt: "rebase",
		CWD:    dir,
		Env:    []string{"CODEX_FAILURE=" + codexRejectedArraySchema},
	})
	if err == nil || IsTerminalProviderError(err) || strings.Contains(err.Error(), "structured-output incompatibility") {
		t.Fatalf("plain-prompt failure must keep its ordinary diagnostic: %v", err)
	}
}

func TestCodexAgent_SchemaRejectionIsNeverRetried(t *testing.T) {
	dir := t.TempDir()
	// Even a retry-looking detail must not replay a request whose schema was
	// explicitly rejected. A zero exit also cannot turn that rejection green.
	bin := writeFakeCodex(t, dir, "#!/bin/sh\nprintf '%s\\n' \"$CODEX_FAILURE\"\n",
		"@echo off\r\necho %CODEX_FAILURE%\r\nexit /b 0\r\n")
	event, err := json.Marshal(map[string]string{
		"type": "error", "message": `{"error":{"message":"connection reset","code":"invalid_json_schema"}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer withFastBackoff(t)()
	attempts := 0
	_, err = (&codexAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        dir,
		JSONSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
		Env:        []string{"CODEX_FAILURE=" + string(event)},
		OnAttempt:  func(Attempt) { attempts++ },
	})
	if !IsTerminalProviderError(err) || attempts != 1 {
		t.Fatalf("Run = %v, attempts = %d; want terminal failure on first attempt", err, attempts)
	}
}

func TestCodexAgent_SuccessIgnoresStderrSchemaDiagnostic(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCodex(t, dir, `#!/bin/sh
printf '%s\n' "$CODEX_FAILURE" >&2
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`, strings.Join([]string{
		"@echo off",
		"echo %CODEX_FAILURE% 1>&2",
		`echo {"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}`,
		`echo {"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
		"exit /b 0",
	}, "\r\n"))
	result, err := (&codexAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        dir,
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		Env:        []string{"CODEX_FAILURE=" + codexRejectedArraySchema},
	})
	if err != nil {
		t.Fatalf("successful structured response must outrank stderr diagnostics: %v", err)
	}
	if result == nil || string(result.Output) != `{"ok":true}` {
		t.Fatalf("result = %+v, want successful structured output", result)
	}
}
