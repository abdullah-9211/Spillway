// Command fakeprovider runs the fake provider as an OpenAI-compatible HTTP server. Point a provider's
// base_url at it (http://localhost:9999/v1) to exercise Spillway without real API keys.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
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
	if *text == "" {
		p.Reply = reply // without -text, answer according to the prompt and model
	}
	srv := &http.Server{Addr: *addr, Handler: fake.NewHandler(p), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("fake provider listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}
