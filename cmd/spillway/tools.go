package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/secret"
	"github.com/abdullah-9211/spillway/internal/tools"
)

const toolsUsage = `usage:
  spillway tools add NAME --endpoint URL [--kind http|mcp] [--description TEXT] [--header "Name: value"]...
                          [--schema FILE_OR_JSON] [--timeout 10s] [--requires-approval] [--discover]
  spillway tools list
  spillway tools discover NAME       fetch an MCP server's tools; they become usable as NAME.tool
  spillway tools rm NAME

Works on the database directly, like keys. Auth headers are encrypted with SPILLWAY_SECRET_KEY.
`

type headerFlags map[string]string

func (h headerFlags) String() string { return "" }
func (h headerFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, ":")
	if !ok || strings.TrimSpace(k) == "" {
		return errors.New(`a header is written "Name: value"`)
	}
	h[strings.TrimSpace(k)] = strings.TrimSpace(val)
	return nil
}

func toolsCmd(ctx context.Context, args []string, getenv func(string) string, stdout, errOut io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(errOut, toolsUsage)
		return errors.New("tools needs a subcommand")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, toolsUsage)
		return nil
	case "add", "list", "discover", "rm":
	default:
		fmt.Fprint(errOut, toolsUsage)
		return fmt.Errorf("unknown tools subcommand %q", sub)
	}
	fs := flag.NewFlagSet("tools "+sub, flag.ContinueOnError)
	fs.SetOutput(errOut)
	dbURL := fs.String("database-url", getenv("DATABASE_URL"), "Postgres URL")
	var endpoint, kind, desc, schema string
	var timeout time.Duration
	var approval, discover bool
	headers := headerFlags{}
	if sub == "add" {
		fs.StringVar(&endpoint, "endpoint", "", "the tool's URL, or the MCP server's endpoint (required)")
		fs.StringVar(&kind, "kind", "http", "http or mcp")
		fs.StringVar(&desc, "description", "", "what the tool does, shown to the model")
		fs.StringVar(&schema, "schema", "", "JSON Schema of the arguments: a file path or inline JSON (http tools)")
		fs.DurationVar(&timeout, "timeout", 0, "per-call timeout (default 10s)")
		fs.BoolVar(&approval, "requires-approval", false, "every call waits for a person to approve")
		fs.BoolVar(&discover, "discover", false, "for an MCP server: fetch its tools right away")
		fs.Var(headers, "header", `an auth header sent on every call, "Name: value" (repeatable); stored encrypted`)
	}
	pos, err := parseInterspersed(fs, rest)
	if err != nil {
		return err
	}
	if *dbURL == "" {
		return errors.New("DATABASE_URL or --database-url is required")
	}
	var box *secret.Box
	if k := getenv("SPILLWAY_SECRET_KEY"); k != "" {
		raw, err := secret.ParseKey(k)
		if err != nil {
			return fmt.Errorf("SPILLWAY_SECRET_KEY: %w", err)
		}
		if box, err = secret.New(raw); err != nil {
			return err
		}
	}
	pg, err := db.OpenPostgres(ctx, *dbURL)
	if err != nil {
		return err
	}
	defer pg.Close()
	store := tools.NewStore(pg.Pool, box)
	exec := &tools.Executor{Source: store}
	name := func() (string, error) {
		if len(pos) != 1 {
			return "", fmt.Errorf("%s needs exactly one tool name", sub)
		}
		return pos[0], nil
	}
	doDiscover := func(t tools.Tool) error {
		got, err := tools.DiscoverAndStore(ctx, store, exec, t.ID, time.Now())
		if err != nil {
			return err
		}
		list, _ := tools.DiscoveredTools(got.InputSchema)
		fmt.Fprintf(stdout, "%s: %d tool(s)\n", t.Name, len(list))
		for _, d := range list {
			fmt.Fprintf(stdout, "  %s.%s  %s\n", t.Name, d.Name, d.Description)
		}
		return nil
	}

	switch sub {
	case "add":
		n, err := name()
		if err != nil {
			return err
		}
		p := tools.CreateParams{Name: n, Kind: tools.Kind(kind), Endpoint: endpoint, Description: desc, Timeout: timeout, RequiresApproval: approval}
		if len(headers) > 0 {
			p.Headers = headers
		}
		if schema != "" {
			if b, err := os.ReadFile(schema); err == nil {
				p.InputSchema = b
			} else {
				p.InputSchema = json.RawMessage(schema)
			}
		}
		t, err := store.Create(ctx, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "registered %s (%s)\n", t.Name, t.Kind)
		if discover && t.Kind == tools.MCP {
			return doDiscover(t)
		}
		return nil
	case "list":
		list, err := store.List(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tKIND\tAPPROVAL\tTOOLS\tENDPOINT")
		for _, t := range list {
			count := "-"
			if t.Kind == tools.MCP {
				l, _ := tools.DiscoveredTools(t.InputSchema)
				count = fmt.Sprint(len(l))
				if t.Discovered.IsZero() {
					count = "not discovered"
				}
			}
			ap := "no"
			if t.RequiresApproval {
				ap = "required"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Kind, ap, count, t.Endpoint)
		}
		return tw.Flush()
	case "discover":
		n, err := name()
		if err != nil {
			return err
		}
		t, err := store.ByName(ctx, n)
		if err != nil {
			return err
		}
		return doDiscover(t)
	case "rm":
		n, err := name()
		if err != nil {
			return err
		}
		t, err := store.ByName(ctx, n)
		if err != nil {
			return err
		}
		if err := store.Delete(ctx, t.ID); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed %s\n", t.Name)
		return nil
	}
	return nil
}
