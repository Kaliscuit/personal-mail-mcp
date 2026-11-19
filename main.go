package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("personal-mail-mcp", flag.ContinueOnError)
	path := flags.String("config", "config.json", "path to the server JSON configuration")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: personal-mail-mcp [-config path] [serve | authorize-outlook ACCOUNT]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	args = flags.Args()
	switch {
	case len(args) == 0 || (len(args) == 1 && args[0] == "serve"):
		c, err := loadConfig(*path)
		if err != nil {
			return err
		}
		token, err := tokenHash(c.TokenFile)
		if err != nil {
			return fmt.Errorf("read MCP token: %w", err)
		}
		endpoints, err := listenAddresses(c.Interfaces, c.Port)
		if err != nil {
			return err
		}
		return serve(ctx, endpoints, mcpHandler(newServer(newMailService(c)), token, c.AdditionalAllowedHosts))
	case len(args) == 2 && args[0] == "authorize-outlook":
		c, err := loadConfig(*path)
		if err != nil {
			return err
		}
		a, ok := c.Accounts[args[1]]
		if !ok || a.Provider != "outlook" {
			return errors.New("authorize-outlook requires a configured Outlook account identifier")
		}
		if err := authorizeOutlook(ctx, a); err != nil {
			return err
		}
		fmt.Println("Outlook authorization saved.")
		return nil
	default:
		flags.Usage()
		return errors.New("invalid command")
	}
}
