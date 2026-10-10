// Command fakeprovider runs the fake provider as an OpenAI-compatible HTTP server. Point a provider's
// base_url at it (http://localhost:9999/v1) to exercise Spillway without real API keys.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/fake"
)

func main() {
	addr := flag.String("addr", ":9999", "listen address")
	text := flag.String("text", "", "one fixed answer for every request (default: an answer that depends on the prompt)")
	delay := flag.Duration("delay", 0, "default latency added to every answer")
	toolName := flag.String("tool", "", "ask for this tool before answering (for trying runs with tools)")
	toolCalls := flag.Int("tool-calls", 3, "how many times to ask for -tool before answering")
	demo := flag.Bool("demo", false, "act as an agent whose script depends on the task: say \"approval\", \"email\", \"sleep\" or \"long\" in it")
	flag.Parse()

	p := fake.New("fake")
	p.Default = fake.Behavior{Text: *text, Delay: *delay}
	if *toolName != "" {
		p.Decide = func(req *provider.ChatRequest) fake.Behavior {
			asked := 0
			for _, m := range req.Messages {
				if m.Role == "assistant" {
					asked++
				}
			}
			if asked < *toolCalls {
				return fake.Behavior{Delay: *delay, ToolCalls: []provider.ToolCall{{ID: fmt.Sprintf("call_%d", asked), Type: "function",
					Function: provider.FunctionCall{Name: *toolName, Arguments: fmt.Sprintf(`{"step":%d}`, asked+1)}}}}
			}
			return fake.Behavior{Delay: *delay, Text: "All done: the tool was called " + fmt.Sprint(asked) + " times."}
		}
	}
	if *demo {
		p.Decide = demoScript(*delay)
	}
	if *text == "" {
		p.Reply = reply // without -text, answer according to the prompt and model
	}
	srv := &http.Server{Addr: *addr, Handler: fake.NewHandler(p), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("fake provider listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}

// demoScript makes the fake model behave like an agent, so the approval, sleep and compaction paths can be tried by
// hand. The task decides the script:
//
//	"approval"  asks a person before it goes on
//	"email"     calls send_email (register it, and list it in approval_required to hold it for approval)
//	"sleep"     sleeps 45 seconds, then finishes
//	"long"      makes 14 calls to the effect tool with big arguments, so the history gets compacted
//
// Anything else answers at once. The summariser call (a system prompt that starts "Summarise") gets a short summary.
func demoScript(delay time.Duration) func(*provider.ChatRequest) fake.Behavior {
	return func(req *provider.ChatRequest) fake.Behavior {
		task, asked := "", 0
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				if strings.HasPrefix(m.Content.PlainText(), "Summarise this conversation") {
					return fake.Behavior{Delay: delay, Text: "Summary: the agent had been working through the task; the earlier steps succeeded and nothing is pending."}
				}
			case "user":
				if task == "" {
					task = strings.ToLower(m.Content.PlainText())
				}
			case "assistant":
				asked++
			}
		}
		call := func(name, args string) fake.Behavior {
			return fake.Behavior{Delay: delay, ToolCalls: []provider.ToolCall{{ID: fmt.Sprintf("call_%d", asked), Type: "function",
				Function: provider.FunctionCall{Name: name, Arguments: args}}}}
		}
		done := func(msg string) fake.Behavior { return fake.Behavior{Delay: delay, Text: msg} }
		switch {
		case strings.Contains(task, "email"):
			if asked == 0 {
				return call("send_email", `{"to":"dana@example.com","subject":"Your refund has been issued","body":"Hi Dana,\n\nWe have refunded $42.00 to your original payment method. It should appear within 3 to 5 days.\n\nThe support team"}`)
			}
			return done("The email to dana@example.com was sent.")
		case strings.Contains(task, "approval"):
			if asked == 0 {
				return call("request_human_approval", `{"reason":"I am about to archive 214 inactive accounts. Please confirm before I go on."}`)
			}
			return done("Done. I went ahead after the approval and archived the accounts.")
		case strings.Contains(task, "sleep"):
			if asked == 0 {
				return call("sleep", `{"seconds":45}`)
			}
			return done("I waited and checked again: the export is ready.")
		case strings.Contains(task, "long"):
			if asked < 14 {
				return call("effect", fmt.Sprintf(`{"i":%d,"notes":%q}`, asked, strings.Repeat("analysis of the quarterly ledger line items ", 90)))
			}
			return done("Finished the long investigation.")
		}
		return done("There was nothing to do for that task.")
	}
}
