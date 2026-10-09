package main

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/abdullah-9211/spillway/internal/provider"
)

// reply writes an answer that depends on the prompt and the model, so trying different prompts and policies in
// the playground looks different each time. It is a stand-in for a real model and says so.
func reply(req *provider.ChatRequest) string {
	prompt := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			prompt = strings.TrimSpace(m.Content.PlainText())
		}
	}
	h := fnv.New32a()
	h.Write([]byte(prompt + req.Model))
	pick := func(opts ...string) string { return opts[int(h.Sum32())%len(opts)] }
	low := strings.ToLower(prompt)

	var body string
	switch {
	case strings.Contains(low, "sql"):
		body = "SELECT date_trunc('month', created_at) AS month, count(DISTINCT user_id) AS mau\nFROM events\nGROUP BY 1\nORDER BY 1;"
	case strings.Contains(low, "summar"):
		body = pick("In short: the release fixes two crashes, speeds up start-up, and drops support for the old config format.", "Three bullets: faster start-up, two crash fixes, one breaking config change.")
	case strings.Contains(low, "translate"), strings.Contains(low, "error"):
		body = "Plain English: the program asked for something it was not allowed to use, so it stopped. Check the permissions and try again."
	case strings.Contains(low, "haiku"), strings.Contains(low, "poem"):
		body = pick("A request goes out,\nthe first door is closed, so it\nwalks to the next one.", "Slow rivers of calls,\na breaker clicks, then waits, then\nlets one drop through.")
	case strings.Contains(low, "joke"):
		body = pick("A load balancer walks into a bar and orders the same drink at every table.", "Why did the request retry? It had a good feeling about the third time.")
	case strings.Contains(low, "circuit"), strings.Contains(low, "breaker"):
		body = "A circuit breaker stops sending requests to a service that keeps failing, so calls do not pile up. After a pause it lets one test request through and only resumes normal traffic if that succeeds."
	default:
		body = pick("Here is a short take on that.", "Good question. The short version follows.", "Sure. In a few words:") + " " + pick("The details depend on your setup, but the idea is simple.", "It comes down to trading a little speed for a lot of reliability.", "Start small, measure, then adjust.")
	}
	quoted := prompt
	if len(quoted) > 60 {
		quoted = quoted[:60] + "…"
	}
	return fmt.Sprintf("%s\n\n(Test answer from the stand-in provider, model %s, to: \"%s\")", body, req.Model, quoted)
}
