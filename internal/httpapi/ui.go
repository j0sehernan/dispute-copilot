package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
	"github.com/j0sehernan/dispute-copilot/internal/export"
	"github.com/j0sehernan/dispute-copilot/internal/intake"
	"github.com/j0sehernan/dispute-copilot/internal/service"
)

var funcs = template.FuncMap{
	"date": func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return "-"
		}
		return t.Format(time.DateOnly)
	},
	"ago": func(t time.Time) string {
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return fmt.Sprintf("%d min ago", int(d.Minutes()))
		case d < 24*time.Hour:
			return fmt.Sprintf("%d h ago", int(d.Hours()))
		}
		return t.Format("Jan 2")
	},
	"statusLabel": func(s any) string { return strings.ReplaceAll(fmt.Sprint(s), "_", " ") },
	"network":     networkName,
	"title": func(s string) string {
		if s == "" {
			return ""
		}
		return strings.ToUpper(s[:1]) + strings.ReplaceAll(s[1:], "_", " ")
	},
}

func parseTemplates() (map[string]*template.Template, error) {
	pages := map[string]*template.Template{}
	for _, name := range []string{"list", "new", "detail"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS,
			"templates/layout.html", "templates/partials.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		pages[name] = t
	}
	return pages, nil
}

// render executes into a buffer first so a template error never sends a
// half-written page.
func (s *Server) render(w http.ResponseWriter, page, tmpl string, status int, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, tmpl, data); err != nil {
		s.log.Error("render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *Server) pageError(w http.ResponseWriter, err error) {
	status := statusFor(err)
	if status == http.StatusInternalServerError {
		s.log.Error("page failed", "err", err)
	}
	http.Error(w, publicMessage(err), status)
}

type listVM struct {
	Rows       []disputeSummary
	Processing bool
}

func (s *Server) listData(r *http.Request) (listVM, error) {
	list, err := s.disputes.List(r.Context())
	if err != nil {
		return listVM{}, err
	}
	vm := listVM{}
	for _, d := range list {
		vm.Rows = append(vm.Rows, summarize(d))
		vm.Processing = vm.Processing || d.Status.Processing()
	}
	return vm, nil
}

func (s *Server) pageList(w http.ResponseWriter, r *http.Request) {
	vm, err := s.listData(r)
	if err != nil {
		s.pageError(w, err)
		return
	}
	s.render(w, "list", "layout.html", http.StatusOK, vm)
}

func (s *Server) fragmentTable(w http.ResponseWriter, r *http.Request) {
	vm, err := s.listData(r)
	if err != nil {
		s.pageError(w, err)
		return
	}
	s.render(w, "list", "table", http.StatusOK, vm)
}

type newVM struct {
	Error string
	Text  string
}

func (s *Server) pageNew(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "new", "layout.html", http.StatusOK, newVM{})
}

func (s *Server) formSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, intake.MaxUploadBytes+64<<10)
	if err := r.ParseMultipartForm(intake.MaxUploadBytes); err != nil {
		s.render(w, "new", "layout.html", http.StatusUnprocessableEntity, newVM{Error: "The upload is too large or malformed."})
		return
	}

	content := []byte(r.FormValue("notice"))
	if f, _, err := r.FormFile("file"); err == nil {
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil {
			s.pageError(w, err)
			return
		}
		if len(data) > 0 {
			content = data
		}
	}

	res, err := s.disputes.Submit(r.Context(), service.Submission{Source: domain.SourceUpload, Content: content})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidInput) {
			s.render(w, "new", "layout.html", http.StatusUnprocessableEntity, newVM{Error: err.Error(), Text: r.FormValue("notice")})
			return
		}
		s.pageError(w, err)
		return
	}
	http.Redirect(w, r, "/disputes/"+res.ID.String(), http.StatusSeeOther)
}

type step struct {
	Label string
	State string // done | current | todo | failed
}

var pipelineSteps = []domain.Status{
	domain.StatusReceived,
	domain.StatusExtracting,
	domain.StatusGatheringEvidence,
	domain.StatusAnalyzing,
	domain.StatusReadyForReview,
	domain.StatusApproved,
}

func steps(d *domain.Dispute, audit []domain.AuditEvent) []step {
	cur := d.Status
	if cur == domain.StatusFailed {
		// Show where it stopped: the last status before failing.
		for i := len(audit) - 1; i >= 0; i-- {
			if audit[i].Kind == domain.AuditStatus && audit[i].Name == string(domain.StatusFailed) {
				if from, ok := audit[i].Detail["from"].(string); ok {
					cur = domain.Status(from)
				}
				break
			}
		}
	}
	// The step we are on is "current", except when the dispute failed there,
	// or when it is the last step (approved), which is simply done.
	curState := "current"
	switch {
	case d.Status == domain.StatusFailed:
		curState = "failed"
	case cur == domain.StatusApproved:
		curState = "done"
	}

	out := make([]step, len(pipelineSteps))
	state := "done"
	for i, s := range pipelineSteps {
		label := strings.ReplaceAll(string(s), "_", " ")
		if s == cur {
			out[i] = step{Label: label, State: curState}
			state = "todo"
			continue
		}
		out[i] = step{Label: label, State: state}
	}
	return out
}

type citedVM struct {
	Text  string
	Cites []string
	Rules []string
}

type requirementVM struct {
	Text        string
	Status      string
	Class       string
	Explanation string
	Cites       []string
}

type evidenceVM struct {
	Source string
	Ref    string
	State  string // found | not_found | not_requested
	JSON   string
}

type auditVM struct {
	At      string
	Step    string
	Kind    string
	Name    string
	Model   string
	Prompt  string
	Tokens  string
	Latency string
	OK      bool
	Note    string
}

type reviewForm struct {
	Reviewer string
	Decision string
	Subject  string
	Body     string
}

type detailVM struct {
	D            *domain.Dispute
	Steps        []step
	Processing   bool
	CanReview    bool
	Fields       [][2]string
	Reasons      []citedVM
	Requirements []requirementVM
	Missing      []string
	Evidence     []evidenceVM
	Audit        []auditVM
	Usage        domain.Usage
	Cost         string
	Form         reviewForm
	Error        string
	Saved        bool
}

func (s *Server) detailData(r *http.Request, v *service.View) detailVM {
	d := v.Dispute
	vm := detailVM{
		D:          d,
		Steps:      steps(d, v.Audit),
		Processing: d.Status.Processing(),
		CanReview:  d.Status == domain.StatusReadyForReview,
		Usage:      v.Usage,
		Cost:       fmt.Sprintf("$%.4f", s.cfg.Pricing.Cost(v.Usage)),
		Form: reviewForm{
			Decision: string(d.Decision),
			Subject:  d.Letter.Subject,
			Body:     d.Letter.Body,
			Reviewer: d.ReviewedBy,
		},
		Saved: r.URL.Query().Get("saved") == "1",
	}
	if vm.Form.Reviewer == "" {
		if c, err := r.Cookie("reviewer"); err == nil {
			vm.Form.Reviewer = c.Value
		}
	}

	if det := d.Details; det != nil {
		due := "-"
		if det.RespondBy != nil {
			due = det.RespondBy.Format("Jan 2, 2006")
		}
		vm.Fields = [][2]string{
			{"Case", det.CaseID},
			{"Network", networkName(det.Network)},
			{"Reason code", det.ReasonCode},
			{"Category", strings.ReplaceAll(string(d.Category), "_", " ")},
			{"Amount", det.Amount.String()},
			{"Transaction", det.TransactionID},
			{"Purchased", det.TransactionDate.Format("Jan 2, 2006")},
			{"Respond by", due},
		}
	}

	if a := d.Analysis; a != nil {
		for _, rs := range a.Reasons {
			c := citedVM{Text: rs.Text, Rules: rs.Rules}
			for _, p := range rs.Evidence {
				c.Cites = append(c.Cites, export.Cite(v.Evidence, p))
			}
			vm.Reasons = append(vm.Reasons, c)
		}
		for _, rq := range a.Requirements {
			row := requirementVM{Text: rq.Text, Status: strings.ReplaceAll(string(rq.Status), "_", " "), Class: string(rq.Status), Explanation: rq.Explanation}
			for _, p := range rq.Evidence {
				row.Cites = append(row.Cites, export.Cite(v.Evidence, p))
			}
			vm.Requirements = append(vm.Requirements, row)
		}
		vm.Missing = a.MissingEvidence
	}

	if !vm.Processing || len(v.Evidence) > 0 {
		for _, src := range domain.EvidenceSources {
			it, ok := v.Evidence[src]
			e := evidenceVM{Source: strings.ReplaceAll(string(src), "_", " "), State: "not_requested"}
			if ok {
				e.Ref = it.Ref
				e.State = "not_found"
				if it.Found {
					e.State = "found"
					var pretty bytes.Buffer
					if json.Indent(&pretty, it.Data, "", "  ") == nil {
						e.JSON = pretty.String()
					}
				}
			}
			vm.Evidence = append(vm.Evidence, e)
		}
	}

	for _, ev := range v.Audit {
		row := auditVM{
			At:     ev.CreatedAt.Format("15:04:05"),
			Step:   ev.Step,
			Kind:   strings.ReplaceAll(string(ev.Kind), "_", " "),
			Name:   ev.Name,
			Model:  ev.Model,
			Prompt: ev.PromptVersion,
			OK:     ev.OK,
		}
		if ev.InputTokens+ev.OutputTokens > 0 {
			row.Tokens = fmt.Sprintf("%d / %d", ev.InputTokens, ev.OutputTokens)
		}
		if ev.Latency > 0 {
			row.Latency = ev.Latency.Round(time.Millisecond).String()
		}
		row.Note = auditNote(ev)
		vm.Audit = append(vm.Audit, row)
	}
	return vm
}

func auditNote(ev domain.AuditEvent) string {
	var parts []string
	for _, k := range []string{"id", "refused", "error", "from", "turn", "attempt", "tool_calls", "reviewer", "policy"} {
		v, ok := ev.Detail[k]
		if !ok || v == "" || v == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	if b, _ := ev.Detail["backstop"].(bool); b {
		parts = append(parts, "backstop")
	}
	if f, ok := ev.Detail["found"].(bool); ok && !f && ev.Kind == domain.AuditToolCall {
		parts = append(parts, "not found")
	}
	return strings.Join(parts, " ")
}

func (s *Server) pageDetail(w http.ResponseWriter, r *http.Request) {
	v, err := s.view(r)
	if err != nil {
		s.pageError(w, err)
		return
	}
	s.render(w, "detail", "layout.html", http.StatusOK, s.detailData(r, v))
}

// fragmentProgress is polled by htmx while the agent works. When the work
// is done it asks the browser to reload the full page.
func (s *Server) fragmentProgress(w http.ResponseWriter, r *http.Request) {
	v, err := s.view(r)
	if err != nil {
		s.pageError(w, err)
		return
	}
	if !v.Dispute.Status.Processing() {
		w.Header().Set("HX-Refresh", "true")
	}
	s.render(w, "detail", "progress", http.StatusOK, s.detailData(r, v))
}

func (s *Server) formReview(w http.ResponseWriter, r *http.Request) {
	id, err := disputeID(r)
	if err != nil {
		s.pageError(w, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.pageError(w, fmt.Errorf("%w: bad form", domain.ErrInvalidInput))
		return
	}
	in := service.ReviewInput{
		Reviewer: r.PostFormValue("reviewer"),
		Decision: r.PostFormValue("decision"),
		Subject:  r.PostFormValue("subject"),
		Body:     r.PostFormValue("body"),
	}
	if in.Reviewer != "" && len(in.Reviewer) <= 100 {
		http.SetCookie(w, &http.Cookie{Name: "reviewer", Value: in.Reviewer, Path: "/", MaxAge: 30 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}

	action := r.PostFormValue("action")
	if action == "approve" {
		err = s.disputes.Approve(r.Context(), id, in)
	} else {
		err = s.disputes.SaveDraft(r.Context(), id, in)
	}
	if err != nil {
		if !errors.Is(err, domain.ErrInvalidInput) {
			s.pageError(w, err)
			return
		}
		v, verr := s.disputes.Get(r.Context(), id)
		if verr != nil {
			s.pageError(w, verr)
			return
		}
		vm := s.detailData(r, v)
		vm.Form = reviewForm(in)
		vm.Error = strings.TrimPrefix(err.Error(), domain.ErrInvalidInput.Error()+": ")
		s.render(w, "detail", "layout.html", http.StatusUnprocessableEntity, vm)
		return
	}

	target := "/disputes/" + id.String()
	if action != "approve" {
		target += "?saved=1"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) downloadMarkdown(w http.ResponseWriter, r *http.Request) {
	pkg, ok := s.pkg(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+pkg.FileName("md")+`"`)
	_, _ = w.Write(pkg.Markdown())
}

func (s *Server) downloadPDF(w http.ResponseWriter, r *http.Request) {
	pkg, ok := s.pkg(w, r)
	if !ok {
		return
	}
	out, err := pkg.PDF()
	if err != nil {
		s.pageError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="`+pkg.FileName("pdf")+`"`)
	_, _ = w.Write(out)
}

func (s *Server) pkg(w http.ResponseWriter, r *http.Request) (export.Package, bool) {
	v, err := s.view(r)
	if err != nil {
		s.pageError(w, err)
		return export.Package{}, false
	}
	if v.Dispute.Analysis == nil {
		s.pageError(w, fmt.Errorf("%w: the analysis is not ready yet", domain.ErrInvalidState))
		return export.Package{}, false
	}
	return export.Package{Dispute: v.Dispute, Evidence: v.Evidence, Audit: v.Audit}, true
}

func networkName(n domain.Network) string {
	switch n {
	case domain.NetworkVisa:
		return "Visa"
	case domain.NetworkMastercard:
		return "Mastercard"
	}
	return string(n)
}
