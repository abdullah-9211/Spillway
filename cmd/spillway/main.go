// Command spillway is the Spillway runtime: `serve`, `migrate`, and (in later phases) `keys` and `seed`.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const helpText = `usage: spillway <command> [flags]

commands:
  serve     run the service (--role=api|worker|all)
  migrate   apply database migrations
  keys      create, list and revoke API keys
  seed      create the dashboard's admin and viewer accounts from SEED_* variables
  runs      create, inspect and cancel runs over the API (create | get | steps | cancel)
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, helpText)
		return 2
	}
	var err error
	switch args[0] {
	case "serve":
		err = serve(ctx, args[1:], getenv, stderr)
	case "migrate":
		err = migrateCmd(args[1:], getenv, stdout, stderr)
	case "keys":
		err = keysCmd(ctx, args[1:], getenv, stdout, stderr)
	case "seed":
		err = seedCmd(ctx, args[1:], getenv, stdout, stderr)
	case "runs":
		err = runsCmd(ctx, args[1:], getenv, os.Stdin, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, helpText)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], helpText)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "spillway:", err)
		return 1
	}
	return 0
}
