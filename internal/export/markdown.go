package export

import (
	"fmt"
	"strings"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
)

func (p Package) Markdown() []byte {
	var b strings.Builder
	d := p.Dispute
	title := d.ID.String()
	if d.Details != nil {
		title = d.Details.CaseID
	}

	fmt.Fprintf(&b, "# Dispute package %s\n\n", title)
	fmt.Fprintf(&b, "**%s**\n\n", p.statusLine())

	if fs := p.fields(); len(fs) > 0 {
		b.WriteString("| | |\n|---|---|\n")
		for _, f := range fs {
			fmt.Fprintf(&b, "| %s | %s |\n", f.label, mdCell(f.value))
		}
		b.WriteString("\n")
	}

	if a := d.Analysis; a != nil {
		b.WriteString("## Decision\n\n")
		fmt.Fprintf(&b, "- Reviewer decision: **%s**\n", decisionLabel(d.Decision))
		fmt.Fprintf(&b, "- Agent recommendation: %s (rule `%s`)\n", decisionLabel(a.Recommendation), a.RuleID)
		if a.PolicyOverride {
			fmt.Fprintf(&b, "- Note: the model suggested %s, but its own requirement check matches the rule's conditions for %s.\n",
				strings.ToLower(decisionLabel(a.ModelRecommendation)), strings.ToLower(decisionLabel(a.Recommendation)))
		}
		b.WriteString("\n### Reasons\n\n")
		for i, r := range a.Reasons {
			fmt.Fprintf(&b, "%d. %s\n", i+1, r.Text)
			for _, e := range r.Evidence {
				fmt.Fprintf(&b, "   - `%s`\n", p.evidenceValue(e))
			}
			for _, rule := range r.Rules {
				fmt.Fprintf(&b, "   - rule `%s`\n", rule)
			}
		}

		b.WriteString("\n## Requirements\n\n| Requirement | Status | Evidence |\n|---|---|---|\n")
		for _, r := range a.Requirements {
			var cites []string
			for _, e := range r.Evidence {
				cites = append(cites, "`"+p.evidenceValue(e)+"`")
			}
			fmt.Fprintf(&b, "| %s | %s | %s<br>%s |\n", mdCell(r.Text), statusLabel(r.Status),
				mdCell(r.Explanation), strings.Join(cites, "<br>"))
		}

		if len(a.MissingEvidence) > 0 {
			b.WriteString("\n## Missing evidence\n\n")
			for _, m := range a.MissingEvidence {
				fmt.Fprintf(&b, "- %s\n", m)
			}
		}
	}

	if d.Letter.Body != "" {
		b.WriteString("\n## Response letter\n\n")
		if d.Letter.Subject != "" {
			fmt.Fprintf(&b, "**Subject:** %s\n\n", d.Letter.Subject)
		}
		b.WriteString(d.Letter.Body)
		b.WriteString("\n")
	}

	b.WriteString("\n## Evidence records\n")
	for _, src := range domain.EvidenceSources {
		it, ok := p.Evidence[src]
		switch {
		case !ok:
			fmt.Fprintf(&b, "\n### %s\n\nNot requested.\n", src)
		case !it.Found:
			fmt.Fprintf(&b, "\n### %s\n\nNot found (looked up `%s`).\n", src, it.Ref)
		default:
			fmt.Fprintf(&b, "\n### %s (`%s`)\n\n```json\n%s\n```\n", src, it.Ref, prettyJSON(it.Data))
		}
	}

	models, prompts, in, out := p.provenance()
	b.WriteString("\n---\n\n")
	fmt.Fprintf(&b, "Prepared with Dispute Copilot. Models: %s. Prompts: %s. Tokens: %d in / %d out.\n",
		orDash(strings.Join(models, ", ")), orDash(strings.Join(prompts, ", ")), in, out)
	b.WriteString("All data in this demo is synthetic.\n")
	return []byte(b.String())
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
