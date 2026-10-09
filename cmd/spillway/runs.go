package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"
)

const runsUsage = `usage:
  spillway runs create [flags] INPUT...      start a run (INPUT is the task; "-" reads it from stdin)
  spillway runs get ID                       show a run
  spillway runs steps ID [--after N]         show a run's step rows
  spillway runs cancel ID                    cancel a run

These talk to a running service over HTTP, as any API client would.
flags (all subcommands): --url (SPILLWAY_URL, default http://localhost:8080)  --key (SPILLWAY_API_KEY)  --json
create flags: --model  --system  --max-steps  --max-cost-usd  --deadline-seconds  --idempotency-key  --wait
`

type runsClient struct {
	base, key string
	http      *http.Client
}

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%s (%d %s)", e.Message, e.Status, e.Code) }

func (c *runsClient) do(ctx context.Context, method, path string, body any, hdr map[string]string) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error.Message == "" {
			e.Error.Message = strings.TrimSpace(string(raw))
		}
		return nil, &apiError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	return raw, nil
}

type runView struct {
	ID              string  `json:"id"`
	Status          string  `json:"status"`
	Model           string  `json:"model"`
	StepCount       int     `json:"step_count"`
	CostUSD         string  `json:"cost_usd"`
	FailureReason   *string `json:"failure_reason"`
	Output          *string `json:"output"`
	CancelRequested bool    `json:"cancel_requested"`
	CreatedAt       string  `json:"created_at"`
	FinishedAt      *string `json:"finished_at"`
}

func (r runView) terminal() bool {
	return r.Status == "succeeded" || r.Status == "failed" || r.Status == "cancelled"
}

func printRun(w io.Writer, r runView) {
	fmt.Fprintf(w, "id:       %s\nstatus:   %s", r.ID, r.Status)
	if r.FailureReason != nil {
		fmt.Fprintf(w, " (%s)", *r.FailureReason)
	}
	if r.CancelRequested && !r.terminal() {
		fmt.Fprint(w, " (cancel requested)")
	}
	fmt.Fprintf(w, "\nmodel:    %s\nsteps:    %d\ncost:     $%s\ncreated:  %s\n", r.Model, r.StepCount, r.CostUSD, r.CreatedAt)
	if r.FinishedAt != nil {
		fmt.Fprintf(w, "finished: %s\n", *r.FinishedAt)
	}
	if r.Output != nil {
		fmt.Fprintf(w, "\n%s\n", *r.Output)
	}
}

func runsCmd(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, errOut io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(errOut, runsUsage)
		return errors.New("runs needs a subcommand")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("runs "+sub, flag.ContinueOnError)
	fs.SetOutput(errOut)
	base := fs.String("url", envOr(getenv, "SPILLWAY_URL", "http://localhost:8080"), "service URL")
	key := fs.String("key", getenv("SPILLWAY_API_KEY"), "API key")
	asJSON := fs.Bool("json", false, "print the raw JSON")
	var model, system, idem, maxCost string
	var maxSteps, deadline int
	var wait bool
	var after int64
	switch sub {
	case "create":
		fs.StringVar(&model, "model", "", "policy or model (default: the service's default policy)")
		fs.StringVar(&system, "system", "", "system prompt")
		fs.IntVar(&maxSteps, "max-steps", 0, "step limit (the server's cap applies)")
		fs.StringVar(&maxCost, "max-cost-usd", "", "cost limit in dollars (the server's cap applies)")
		fs.IntVar(&deadline, "deadline-seconds", 0, "deadline in seconds (the server's cap applies)")
		fs.StringVar(&idem, "idempotency-key", "", "dedupes creation: the same key and body returns the same run")
		fs.BoolVar(&wait, "wait", false, "wait for the run to end, printing each status change")
	case "steps":
		fs.Int64Var(&after, "after", 0, "only rows with an id greater than this")
	case "get", "cancel":
	case "-h", "--help", "help":
		fmt.Fprint(stdout, runsUsage)
		return nil
	default:
		fmt.Fprint(errOut, runsUsage)
		return fmt.Errorf("unknown runs subcommand %q", sub)
	}
	pos, err := parseInterspersed(fs, rest)
	if err != nil {
		return err
	}
	if *key == "" {
		return errors.New("an API key is required: --key or SPILLWAY_API_KEY (create one with `spillway keys create`)")
	}
	c := &runsClient{base: strings.TrimRight(*base, "/"), key: *key, http: &http.Client{Timeout: 30 * time.Second}}

	get := func(id string) (runView, []byte, error) {
		raw, err := c.do(ctx, "GET", "/v1/runs/"+url.PathEscape(id), nil, nil)
		var v runView
		if err == nil {
			err = json.Unmarshal(raw, &v)
		}
		return v, raw, err
	}
	needID := func() (string, error) {
		if len(pos) != 1 {
			return "", fmt.Errorf("%s needs exactly one run id", sub)
		}
		return pos[0], nil
	}

	switch sub {
	case "create":
		input := strings.Join(pos, " ")
		if input == "-" {
			b, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
			if err != nil {
				return err
			}
			input = strings.TrimSpace(string(b))
		}
		if strings.TrimSpace(input) == "" {
			return errors.New("give the task as arguments, or \"-\" to read it from stdin")
		}
		body := map[string]any{"input": input}
		if model != "" {
			body["model"] = model
		}
		if system != "" {
			body["system"] = system
		}
		limits := map[string]any{}
		if maxSteps > 0 {
			limits["max_steps"] = maxSteps
		}
		if maxCost != "" {
			limits["max_cost_usd"] = json.Number(maxCost)
		}
		if deadline > 0 {
			limits["deadline_seconds"] = deadline
		}
		if len(limits) > 0 {
			body["limits"] = limits
		}
		hdr := map[string]string{}
		if idem != "" {
			hdr["Idempotency-Key"] = idem
		}
		raw, err := c.do(ctx, "POST", "/v1/runs", body, hdr)
		if err != nil {
			return err
		}
		var created struct{ ID, Status string }
		if err := json.Unmarshal(raw, &created); err != nil {
			return err
		}
		if !wait {
			if *asJSON {
				fmt.Fprintln(stdout, string(raw))
			} else {
				fmt.Fprintf(stdout, "id:     %s\nstatus: %s\n", created.ID, created.Status)
			}
			return nil
		}
		return waitRun(ctx, c, created.ID, *asJSON, stdout, get)

	case "get":
		id, err := needID()
		if err != nil {
			return err
		}
		v, raw, err := get(id)
		if err != nil {
			return err
		}
		if *asJSON {
			fmt.Fprintln(stdout, string(raw))
		} else {
			printRun(stdout, v)
		}
		return nil

	case "cancel":
		id, err := needID()
		if err != nil {
			return err
		}
		raw, err := c.do(ctx, "POST", "/v1/runs/"+url.PathEscape(id)+"/cancel", nil, nil)
		if err != nil {
			return err
		}
		if *asJSON {
			fmt.Fprintln(stdout, string(raw))
			return nil
		}
		var v runView
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		printRun(stdout, v)
		return nil

	case "steps":
		id, err := needID()
		if err != nil {
			return err
		}
		var all []stepView
		cursor := after
		for {
			raw, err := c.do(ctx, "GET", fmt.Sprintf("/v1/runs/%s/steps?after=%d&limit=500", url.PathEscape(id), cursor), nil, nil)
			if err != nil {
				return err
			}
			var page struct {
				Steps     []stepView `json:"steps"`
				NextAfter int64      `json:"next_after"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return err
			}
			all = append(all, page.Steps...)
			if len(page.Steps) < 500 {
				break
			}
			cursor = page.NextAfter
		}
		if *asJSON {
			return json.NewEncoder(stdout).Encode(map[string]any{"steps": all})
		}
		printSteps(stdout, all)
		return nil
	}
	return nil
}

type stepView struct {
	ID             int64           `json:"id"`
	StepNo         *int            `json:"step_no"`
	Type           string          `json:"type"`
	Phase          string          `json:"phase"`
	IdempotencyKey string          `json:"idempotency_key"`
	Payload        json.RawMessage `json:"payload"`
	CostUSD        string          `json:"cost_usd"`
	WorkerID       string          `json:"worker_id"`
	LeaseEpoch     int64           `json:"lease_epoch"`
}

func printSteps(w io.Writer, steps []stepView) {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTEP\tTYPE\tPHASE\tWORKER\tEPOCH\tCOST\tDETAIL")
	for _, s := range steps {
		no := "-"
		if s.StepNo != nil {
			no = fmt.Sprint(*s.StepNo)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%d\t$%s\t%s\n", s.ID, no, s.Type, s.Phase, orDash(s.WorkerID), s.LeaseEpoch, s.CostUSD, detail(s))
	}
	tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// detail is the one thing worth reading in a row's payload.
func detail(s stepView) string {
	var p map[string]any
	if json.Unmarshal(s.Payload, &p) != nil {
		return ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := p[k]; ok {
				return fmt.Sprintf("%s=%v", k, v)
			}
		}
		return ""
	}
	var out string
	switch {
	case s.Type == "run_status":
		out = pick("status")
		if r, ok := p["reason"]; ok {
			out += fmt.Sprintf(" reason=%v", r)
		}
	case s.Phase == "reissued":
		out = fmt.Sprintf("after %v (epoch %v)", p["previous_worker"], p["previous_epoch"])
	case s.Phase == "failed":
		out = pick("error")
	case s.Type == "tool_call" && s.Phase == "started":
		out = pick("tool")
	case s.Type == "tool_call":
		out = pick("result")
	case s.Type == "model_call" && s.Phase == "finished":
		out = pick("model", "provider")
	case s.Type == "model_call":
		out = pick("policy")
	}
	if len(out) > 80 {
		out = out[:77] + "..."
	}
	return out
}

func waitRun(ctx context.Context, c *runsClient, id string, asJSON bool, stdout io.Writer, get func(string) (runView, []byte, error)) error {
	last := ""
	for {
		v, raw, err := get(id)
		if err != nil {
			return err
		}
		if v.Status != last && !asJSON {
			fmt.Fprintf(stdout, "%s  %s\n", time.Now().Format("15:04:05"), v.Status)
			last = v.Status
		}
		if v.terminal() {
			if asJSON {
				fmt.Fprintln(stdout, string(raw))
			} else {
				fmt.Fprintln(stdout)
				printRun(stdout, v)
			}
			if v.Status != "succeeded" {
				return fmt.Errorf("the run %s", v.Status)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// parseInterspersed is flag parsing that accepts flags before, between and after the positional words, so
// `runs create say hello --model mini` works. A bare "--" ends the flags.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}
