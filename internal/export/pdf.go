package export

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-pdf/fpdf"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
)

func (p Package) PDF() ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(18, 18, 18)
	pdf.SetAutoPageBreak(true, 18)
	// Core fonts are cp1252; translate so names like "Tomás" print right.
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	w, _ := pdf.GetPageSize()
	width := w - 36

	pdf.SetFooterFunc(func() {
		pdf.SetY(-12)
		pdf.SetFont("Helvetica", "", 8)
		pdf.SetTextColor(120, 120, 120)
		pdf.CellFormat(0, 5, tr("Dispute Copilot - synthetic demo data"), "", 0, "L", false, 0, "")
		pdf.CellFormat(0, 5, fmt.Sprintf("Page %d", pdf.PageNo()), "", 0, "R", false, 0, "")
	})
	pdf.AddPage()

	heading := func(s string) {
		pdf.Ln(4)
		pdf.SetFont("Helvetica", "B", 12)
		pdf.SetTextColor(20, 20, 20)
		pdf.MultiCell(width, 6, tr(s), "", "L", false)
		pdf.Ln(1)
	}
	text := func(s string) {
		pdf.SetFont("Helvetica", "", 10)
		pdf.SetTextColor(40, 40, 40)
		pdf.MultiCell(width, 5, tr(s), "", "L", false)
	}

	d := p.Dispute
	title := d.ID.String()
	if d.Details != nil {
		title = d.Details.CaseID
	}
	pdf.SetFont("Helvetica", "B", 16)
	pdf.MultiCell(width, 8, tr("Dispute package "+title), "", "L", false)
	pdf.SetFont("Helvetica", "", 10)
	if d.Status == domain.StatusApproved {
		pdf.SetTextColor(20, 110, 60)
	} else {
		pdf.SetTextColor(170, 90, 0)
	}
	pdf.MultiCell(width, 6, tr(p.statusLine()), "", "L", false)
	pdf.Ln(2)

	for _, f := range p.fields() {
		pdf.SetFont("Helvetica", "B", 9)
		pdf.SetTextColor(90, 90, 90)
		pdf.CellFormat(38, 5, tr(f.label), "", 0, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 10)
		pdf.SetTextColor(30, 30, 30)
		pdf.MultiCell(width-38, 5, tr(f.value), "", "L", false)
	}

	if a := d.Analysis; a != nil {
		heading("Decision")
		text(fmt.Sprintf("Reviewer decision: %s. Agent recommendation: %s (rule %s).",
			decisionLabel(d.Decision), decisionLabel(a.Recommendation), a.RuleID))
		for i, r := range a.Reasons {
			text(fmt.Sprintf("%d. %s", i+1, r.Text))
			for _, e := range r.Evidence {
				pdf.SetFont("Courier", "", 8)
				pdf.MultiCell(width, 4, tr("     "+p.evidenceValue(e)), "", "L", false)
			}
		}

		heading("Requirements")
		for _, r := range a.Requirements {
			pdf.SetFont("Helvetica", "B", 10)
			pdf.SetTextColor(30, 30, 30)
			pdf.MultiCell(width, 5, tr(fmt.Sprintf("[%s] %s", statusLabel(r.Status), r.Text)), "", "L", false)
			text(r.Explanation)
			pdf.Ln(1)
		}
		if len(a.MissingEvidence) > 0 {
			heading("Missing evidence")
			for _, m := range a.MissingEvidence {
				text("- " + m)
			}
		}
	}

	if d.Letter.Body != "" {
		heading("Response letter")
		if d.Letter.Subject != "" {
			pdf.SetFont("Helvetica", "B", 10)
			pdf.MultiCell(width, 5, tr(d.Letter.Subject), "", "L", false)
			pdf.Ln(1)
		}
		text(d.Letter.Body)
	}

	pdf.AddPage()
	heading("Evidence records")
	for _, src := range domain.EvidenceSources {
		it, ok := p.Evidence[src]
		pdf.SetFont("Helvetica", "B", 10)
		pdf.SetTextColor(30, 30, 30)
		switch {
		case !ok:
			pdf.MultiCell(width, 5, tr(string(src)+": not requested"), "", "L", false)
		case !it.Found:
			pdf.MultiCell(width, 5, tr(fmt.Sprintf("%s: not found (%s)", src, it.Ref)), "", "L", false)
		default:
			pdf.MultiCell(width, 5, tr(fmt.Sprintf("%s (%s)", src, it.Ref)), "", "L", false)
			pdf.SetFont("Courier", "", 7.5)
			pdf.SetTextColor(50, 50, 50)
			pdf.MultiCell(width, 3.4, tr(prettyJSON(it.Data)), "", "L", false)
		}
		pdf.Ln(2)
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func prettyJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return strings.TrimSpace(string(b))
}
