// Package rules holds the dispute rule library and the retrieval (RAG) on
// top of it. The rule texts in corpus/ are short summaries written for this
// project; they are not the card networks' rules and should not be used as
// such.
package rules

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
)

//go:embed corpus/*.md
var corpus embed.FS

type Requirement struct {
	ID   string `yaml:"id" json:"id"`
	Text string `yaml:"text" json:"text"`
}

// Doc is one rule summary (one reason code) or one general guide. Guides
// have no requirements.
type Doc struct {
	ID           string          `yaml:"id"`
	Network      string          `yaml:"network"`
	ReasonCode   string          `yaml:"reason_code"`
	Category     domain.Category `yaml:"category"`
	Title        string          `yaml:"title"`
	Requirements []Requirement   `yaml:"requirements"`
	FightWhen    [][]string      `yaml:"fight_when"`

	Chunks []Chunk `yaml:"-"`
}

func (d Doc) IsRule() bool { return len(d.Requirements) > 0 }

func (d Doc) Policy() domain.Policy { return domain.Policy{FightWhen: d.FightWhen} }

// Chunk is one "## " section of a doc, the unit we embed and cite.
type Chunk struct {
	ID      string // e.g. "visa-10.4#what-wins"
	DocID   string
	Network string
	Heading string
	Content string
}

// EmbedText is what gets embedded: the doc title gives the section context.
func (c Chunk) EmbedText(docTitle string) string {
	return docTitle + " - " + c.Heading + "\n" + c.Content
}

func (c Chunk) Hash() string {
	sum := sha256.Sum256([]byte(c.Heading + "\x00" + c.Content))
	return hex.EncodeToString(sum[:8])
}

type Library struct {
	docs   map[string]*Doc
	byCode map[string]string
}

// Load parses the embedded corpus.
func Load() (*Library, error) {
	entries, err := corpus.ReadDir("corpus")
	if err != nil {
		return nil, err
	}
	lib := &Library{docs: map[string]*Doc{}, byCode: map[string]string{}}
	for _, e := range entries {
		raw, err := corpus.ReadFile(path.Join("corpus", e.Name()))
		if err != nil {
			return nil, err
		}
		doc, err := parse(raw)
		if err != nil {
			return nil, fmt.Errorf("rules: %s: %w", e.Name(), err)
		}
		if _, dup := lib.docs[doc.ID]; dup {
			return nil, fmt.Errorf("rules: duplicate doc id %q", doc.ID)
		}
		lib.docs[doc.ID] = doc
		if doc.IsRule() {
			lib.byCode[codeKey(doc.Network, doc.ReasonCode)] = doc.ID
		}
	}
	return lib, nil
}

func codeKey(network, code string) string {
	return strings.ToLower(network) + ":" + strings.TrimSpace(code)
}

func (l *Library) ByReasonCode(network domain.Network, code string) (*Doc, bool) {
	id, ok := l.byCode[codeKey(string(network), code)]
	if !ok {
		return nil, false
	}
	return l.docs[id], true
}

func (l *Library) Doc(id string) (*Doc, bool) {
	d, ok := l.docs[id]
	return d, ok
}

// Docs returns all docs sorted by id.
func (l *Library) Docs() []*Doc {
	out := make([]*Doc, 0, len(l.docs))
	for _, d := range l.docs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func parse(raw []byte) (*Doc, error) {
	front, body, err := splitFrontMatter(raw)
	if err != nil {
		return nil, err
	}
	var doc Doc
	if err := yaml.Unmarshal(front, &doc); err != nil {
		return nil, err
	}
	if doc.ID == "" || doc.Title == "" || doc.Network == "" {
		return nil, errors.New("id, title and network are required")
	}
	if err := doc.validatePolicy(); err != nil {
		return nil, err
	}
	doc.Chunks = sections(doc, body)
	if len(doc.Chunks) == 0 {
		return nil, errors.New("doc has no sections")
	}
	return &doc, nil
}

func (d Doc) validatePolicy() error {
	if !d.IsRule() {
		return nil
	}
	if d.ReasonCode == "" || d.Category == "" {
		return errors.New("rule docs need reason_code and category")
	}
	known := map[string]bool{}
	for _, r := range d.Requirements {
		known[r.ID] = true
	}
	if len(d.FightWhen) == 0 {
		return errors.New("rule docs need fight_when")
	}
	for _, group := range d.FightWhen {
		for _, id := range group {
			if !known[id] {
				return fmt.Errorf("fight_when references unknown requirement %q", id)
			}
		}
	}
	return nil
}

func splitFrontMatter(raw []byte) (front, body []byte, err error) {
	const sep = "---\n"
	if !bytes.HasPrefix(raw, []byte(sep)) {
		return nil, nil, errors.New("missing front matter")
	}
	rest := raw[len(sep):]
	end := bytes.Index(rest, []byte("\n"+sep))
	if end < 0 {
		return nil, nil, errors.New("unterminated front matter")
	}
	return rest[:end], rest[end+len(sep)+1:], nil
}

func sections(doc Doc, body []byte) []Chunk {
	var chunks []Chunk
	var heading string
	var buf strings.Builder

	flush := func() {
		text := strings.TrimSpace(buf.String())
		if heading != "" && text != "" {
			chunks = append(chunks, Chunk{
				ID:      doc.ID + "#" + slug(heading),
				DocID:   doc.ID,
				Network: doc.Network,
				Heading: heading,
				Content: text,
			})
		}
		buf.Reset()
	}

	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if h, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			heading = strings.TrimSpace(h)
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	flush()
	return chunks
}

func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
