// Command seed loads the synthetic disputes into a running stack. Running
// it twice is safe: deliveries are deduplicated by content.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/j0sehernan/dispute-copilot/internal/apiclient"
	"github.com/j0sehernan/dispute-copilot/internal/fixtures"
)

func main() {
	api := flag.String("api", envOr("API_URL", "http://localhost:8080"), "API base URL")
	flag.Parse()

	if err := run(*api); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run(api string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	scenarios, err := fixtures.Load()
	if err != nil {
		return err
	}
	client := apiclient.New(api, os.Getenv("WEBHOOK_SECRET"))
	if err := client.WaitReady(ctx); err != nil {
		return err
	}

	created := 0
	for _, sc := range scenarios {
		res, err := client.Submit(ctx, sc, "")
		if err != nil {
			return err
		}
		state := "queued"
		if res.Duplicate {
			state = "already there"
		} else {
			created++
		}
		fmt.Printf("%-42s %-5s %s  %s\n", sc.ID, sc.Input.Kind, res.ID, state)
	}
	fmt.Printf("\n%d new disputes queued. Open %s/disputes to follow them.\n", created, envOr("PUBLIC_URL", api))
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
