package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"os"
	"os/exec"
	"time"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func main() {
	mode, dir := os.Args[1], os.Args[2]
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ag, err := agent.NewWithOptions(types.AgentCodex, os.Getenv("LAB_CODEX"), []string{"-p", "deepseek", "--skip-git-repo-check"}, agent.Options{DisableProjectSettings: true})
	must(err)
	defer ag.Close()
	attempts := 0
	opts := agent.RunOpts{Prompt: "Review the change. Return only the requested JSON, or plain-ok if no JSON schema is requested.", CWD: dir, OnAttempt: func(a agent.Attempt) { attempts++ }}
	if mode != "plain" {
		opts.JSONSchema = json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object","properties":{"description":{"type":"string"}},"required":["description"]}},"note":{"type":"string"}},"required":["findings"]}`)
	}
	var output any
	if mode == "review" {
		database, e := db.Open(os.Getenv("LAB_DB"))
		must(e)
		defer database.Close()
		repo, e := database.InsertRepo(dir, dir, "main")
		must(e)
		run, e := database.InsertRun(repo.ID, "refs/heads/feature", os.Getenv("LAB_HEAD"), os.Getenv("LAB_BASE"))
		must(e)
		sctx := &pipeline.StepContext{Ctx: ctx, Run: run, Repo: repo, WorkDir: dir, DB: database, Agent: ag, Config: &config.Config{Agent: types.AgentCodex}, Log: func(s string) { fmt.Println("review:", s) }, LogFile: func(string) {}, LogChunk: func(s string) { fmt.Print(s) }}
		output, err = (&steps.ReviewStep{}).Execute(sctx)
	} else {
		output, err = ag.Run(ctx, opts)
	}
	var exit *exec.ExitError
	code := 0
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	must(json.NewEncoder(os.Stdout).Encode(map[string]any{"mode": mode, "error": message, "terminal_provider_error": agent.IsTerminalProviderError(err), "structured_output_rejected": agent.IsStructuredOutputRejected(err), "subprocess_exit": code, "attempts": attempts, "result": output}))
}
