// Command eval runs every synthetic dispute through a running stack and
// compares the agent's output with the expected labels.
//
// It talks to the API like any client would, so it measures the real
// system: queue, worker, model, retrieval and validation.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/j0sehernan/dispute-copilot/internal/apiclient"
	"github.com/j0sehernan/dispute-copilot/internal/fixtures"
)

type result struct {
	sc          fixtures.Scenario
	d           apiclient.Dispute
	err         error
	categoryOK  bool
	decisionOK  bool
	evFound     int
	evExpected  int
	backstopped int
	model       string
}

func main() {
	var (
		api     = flag.String("api", envOr("API_URL", "http://localhost:8080"), "API base URL")
		timeout = flag.Duration("timeout", 45*time.Minute, "max time for the whole run")
		only    = flag.String("only", "", "run only scenarios whose id contains this text")
		out     = flag.String("out", "", "also write the results as Markdown to this file")
		readme  = flag.String("readme", "", "replace the eval section of this README with the results")
	)
	flag.Parse()

	if err := run(*api, *only, *out, *readme, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		os.Exit(1)
	}
}

func run(api, only, out, readme string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	scenarios, err := fixtures.Load()
	if err != nil {
		return err
	}
	if only != "" {
		var keep []fixtures.Scenario
		for _, s := range scenarios {
			if strings.Contains(s.ID, only) {
				keep = append(keep, s)
			}
		}
		scenarios = keep
	}

	client := apiclient.New(api, os.Getenv("WEBHOOK_SECRET"))
	if err := client.WaitReady(ctx); err != nil {
		return err
	}

	// A fresh idempotency key per run, so every run analyses from scratch.
	runID := time.Now().UTC().Format("20060102T150405")
	fmt.Printf("eval run %s: %d disputes\n\n", runID, len(scenarios))

	results := make([]result, len(scenarios))
	var wg sync.WaitGroup
	for i, sc := range scenarios {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = evaluate(ctx, client, sc, "eval-"+runID+"-"+sc.ID)
			r := results[i]
			fmt.Printf("  done %-42s %s\n", sc.ID, statusOf(r))
		}()
	}
	wg.Wait()

	report := render(results)
	fmt.Println()
	fmt.Print(report.text)
	if out != "" {
		if err := os.WriteFile(out, []byte(report.markdown), 0o644); err != nil {
			return err
		}
		fmt.Printf("\nwrote %s\n", out)
	}
	if readme != "" {
		if err := updateReadme(readme, report.markdown, modelOf(results), runID); err != nil {
			return err
		}
		fmt.Printf("updated %s\n", readme)
	}
	return nil
}

const (
	startMarker = "<!-- eval:start -->"
	endMarker   = "<!-- eval:end -->"
)

// updateReadme swaps the text between the eval markers for the new table.
func updateReadme(path, table, model, runID string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := string(b)
	start, end := strings.Index(s, startMarker), strings.Index(s, endMarker)
	if start < 0 || end < start {
		return fmt.Errorf("%s has no eval markers", path)
	}
	section := fmt.Sprintf("%s\nRun `%s`, model `%s`.\n\n%s%s", startMarker, runID, model, table, endMarker)
	return os.WriteFile(path, []byte(s[:start]+section+s[end+len(endMarker):]), 0o644)
}

func modelOf(rs []result) string {
	for _, r := range rs {
		if r.model != "" {
			return r.model
		}
	}
	return "unknown"
}

func evaluate(ctx context.Context, c *apiclient.Client, sc fixtures.Scenario, key string) result {
	r := result{sc: sc, evExpected: len(sc.Expected.Evidence)}
	sub, err := c.Submit(ctx, sc, key)
	if err != nil {
		r.err = err
		return r
	}
	d, err := c.Wait(ctx, sub.ID, 2*time.Second)
	if err != nil {
		r.err = err
		return r
	}
	r.d = d
	if d.Status == "failed" {
		r.err = fmt.Errorf("failed: %s", d.FailureReason)
		return r
	}

	r.categoryOK = d.Category == sc.Expected.Category
	r.decisionOK = d.Recommendation == sc.Expected.Recommendation
	for _, src := range sc.Expected.Evidence {
		if d.Evidence[src].Found {
			r.evFound++
		}
	}
	if events, err := c.Audit(ctx, sub.ID); err == nil {
		for _, ev := range events {
			if b, _ := ev.Detail["backstop"].(bool); b && ev.Kind == "tool_call" {
				r.backstopped++
			}
			if ev.Kind == "llm_call" && ev.Model != "" {
				r.model = ev.Model
			}
		}
	}
	return r
}

func statusOf(r result) string {
	if r.err != nil {
		return "ERROR " + r.err.Error()
	}
	return fmt.Sprintf("%s (%s)", r.d.Recommendation, time.Duration(r.d.ProcessingMS)*time.Millisecond)
}

type report struct{ text, markdown string }

func render(rs []result) report {
	var t, md strings.Builder
	fmt.Fprintf(&t, "%-42s %-5s %-9s %-14s %-8s %-8s %-13s %-9s %-9s %s\n",
		"scenario", "input", "category", "decision", "evidence", "backstop", "tokens in/out", "cost", "model", "end-to-end")
	md.WriteString("| Scenario | Input | Category | Decision (expected) | Evidence found | Tokens in / out | Cost | Model time | End-to-end |\n")
	md.WriteString("|---|---|---|---|---|---|---|---|---|\n")

	var (
		catOK, decOK, failed, evFound, evExpected, inTok, outTok int
		cost                                                     float64
		latencies, modelTimes                                    []time.Duration
	)
	for _, r := range rs {
		evExpected += r.evExpected
		if r.err != nil {
			failed++
			fmt.Fprintf(&t, "%-42s %-5s ERROR %s\n", r.sc.ID, r.sc.Input.Kind, r.err)
			fmt.Fprintf(&md, "| %s | %s | error | %s | - | - | - | - | - |\n", r.sc.ID, r.sc.Input.Kind, mdEscape(r.err.Error()))
			continue
		}
		if r.categoryOK {
			catOK++
		}
		if r.decisionOK {
			decOK++
		}
		evFound += r.evFound
		u := r.d.Usage
		inTok += u.InputTokens
		outTok += u.OutputTokens
		cost += u.CostUSD
		e2e := time.Duration(r.d.ProcessingMS) * time.Millisecond
		model := time.Duration(u.LLMLatencyMS) * time.Millisecond
		latencies = append(latencies, e2e)
		modelTimes = append(modelTimes, model)

		decision := mark(r.decisionOK) + " " + r.d.Recommendation
		if r.d.Analysis != nil && r.d.Analysis.PolicyOverride {
			decision += "*"
		}
		fmt.Fprintf(&t, "%-42s %-5s %-9s %-14s %-8s %-8d %-13s %-9s %-9s %s\n",
			r.sc.ID, r.sc.Input.Kind, mark(r.categoryOK), decision,
			fmt.Sprintf("%d/%d", r.evFound, r.evExpected), r.backstopped,
			fmt.Sprintf("%d/%d", u.InputTokens, u.OutputTokens), fmt.Sprintf("$%.4f", u.CostUSD),
			round(model), round(e2e))
		fmt.Fprintf(&md, "| %s | %s | %s | %s (%s) | %d/%d | %d / %d | $%.4f | %s | %s |\n",
			r.sc.ID, r.sc.Input.Kind, mark(r.categoryOK), decision, r.sc.Expected.Recommendation,
			r.evFound, r.evExpected, u.InputTokens, u.OutputTokens, u.CostUSD, round(model), round(e2e))
	}

	n := len(rs)
	done := n - failed
	summary := fmt.Sprintf(
		"category accuracy  %d/%d (%s)\n"+
			"decision accuracy  %d/%d (%s)\n"+
			"evidence recall    %d/%d (%s)\n"+
			"failed             %d\n"+
			"tokens             %d in / %d out\n"+
			"cost               $%.4f total, $%.4f per dispute\n"+
			"model time         median %s, p95 %s\n"+
			"end-to-end         median %s, p95 %s (includes queue wait; all disputes were submitted at once)\n",
		catOK, n, pct(catOK, n), decOK, n, pct(decOK, n), evFound, evExpected, pct(evFound, evExpected),
		failed, inTok, outTok, cost, cost/float64(max(done, 1)),
		round(percentile(modelTimes, 50)), round(percentile(modelTimes, 95)),
		round(percentile(latencies, 50)), round(percentile(latencies, 95)))

	t.WriteString("\n" + summary)
	t.WriteString("* the model's own answer differed from the rule policy; the policy was applied\n")
	md.WriteString("\n```\n" + summary + "```\n")
	return report{text: t.String(), markdown: md.String()}
}

func mark(ok bool) string {
	if ok {
		return "ok"
	}
	return "MISS"
}

func pct(a, b int) string {
	if b == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(a)/float64(b))
}

func percentile(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := (p*len(s)+99)/100 - 1
	return s[max(0, min(idx, len(s)-1))]
}

func round(d time.Duration) string {
	return d.Round(100 * time.Millisecond).String()
}

func mdEscape(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
