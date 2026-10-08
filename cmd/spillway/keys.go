package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/abdullah-9211/spillway/internal/db"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
)

const keysUsage = `usage:
  spillway keys create --name NAME [--rpm N] [--budget-usd 5.00]
  spillway keys list
  spillway keys revoke ID_OR_PREFIX
`

func keysCmd(ctx context.Context, args []string, getenv func(string) string, stdout, errOut io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(errOut, keysUsage)
		return errors.New("keys needs a subcommand")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("keys "+sub, flag.ContinueOnError)
	fs.SetOutput(errOut)
	dbURL := fs.String("database-url", getenv("DATABASE_URL"), "Postgres URL")
	var name, budget string
	var rpm int
	if sub == "create" {
		fs.StringVar(&name, "name", "", "key name (required)")
		fs.IntVar(&rpm, "rpm", 0, "requests per minute; omit for unlimited")
		fs.StringVar(&budget, "budget-usd", "", "monthly budget in dollars; omit for unlimited")
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *dbURL == "" {
		return errors.New("DATABASE_URL or --database-url is required")
	}
	pg, err := db.OpenPostgres(ctx, *dbURL)
	if err != nil {
		return err
	}
	defer pg.Close()
	store := keys.NewStore(pg.Pool)

	switch sub {
	case "create":
		p := keys.CreateParams{Name: name}
		if rpm != 0 {
			p.RateLimitRPM = &rpm
		}
		if budget != "" {
			m, err := money.ParseUSD(budget)
			if err != nil {
				return fmt.Errorf("--budget-usd: %w", err)
			}
			p.MonthlyBudget = &m
		}
		k, full, err := store.Create(ctx, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "id:   %s\nname: %s\nkey:  %s\n\nCopy the key now. It is not stored and cannot be shown again.\n", k.ID, k.Name, full)
		return nil

	case "list":
		list, err := store.List(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tPREFIX\tNAME\tRPM\tBUDGET (USD/mo)\tSTATUS\tCREATED")
		for _, k := range list {
			rpmS, budS, status := "unlimited", "unlimited", "active"
			if k.RateLimitRPM != nil {
				rpmS = fmt.Sprint(*k.RateLimitRPM)
			}
			if k.MonthlyBudget != nil {
				budS = k.MonthlyBudget.String()
			}
			if k.RevokedAt != nil {
				status = "revoked"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", k.ID, k.Prefix, k.Name, rpmS, budS, status, k.CreatedAt.Format(time.DateTime))
		}
		return tw.Flush()

	case "revoke":
		if fs.NArg() != 1 {
			fmt.Fprint(errOut, keysUsage)
			return errors.New("revoke needs one key id or prefix")
		}
		k, err := store.Revoke(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "revoked %s (%s)\n", k.Prefix, k.Name)
		return nil
	}
	fmt.Fprint(errOut, keysUsage)
	return fmt.Errorf("unknown keys subcommand %q", sub)
}
