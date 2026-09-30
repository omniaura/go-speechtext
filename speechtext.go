// Package speechtext turns Markdown and HTML into plain, spoken text.
// Formatting is deterministic. Callers may provide hooks for content that
// needs an application-specific description, such as a fenced code block.
package speechtext

import (
	"context"
	"fmt"
	stdhtml "html"
	"strings"
	"sync"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Kind identifies a self-contained block that can be handled by a hook.
type Kind string

const (
	KindCode Kind = "code" // Fenced and indented code blocks.
	KindHTML Kind = "html" // Block-level HTML.
)

// Block is the original content passed to a hook. Language is set for fenced
// code blocks when the fence has an info string.
type Block struct {
	Kind     Kind
	Content  string
	Language string
}

// Hook replaces one block with spoken text. An empty result omits the block.
// Hooks may run concurrently and must be safe for concurrent calls. The
// returned text is treated as plain text, never parsed as Markdown or HTML.
type Hook func(context.Context, Block) (string, error)

// Options configures optional hooks. Without a code hook, code blocks are
// omitted. Without an HTML hook, HTML blocks are converted to visible text.
// Parallelism limits simultaneous hook calls; zero uses four workers.
type Options struct {
	Hooks       map[Kind]Hook
	Parallelism int
}

// Formatter is safe for concurrent Format calls. Configure its hooks before
// constructing it; New copies the hooks map.
type Formatter struct {
	hooks       map[Kind]Hook
	parallelism int
}

// New constructs a formatter with optional block hooks.
func New(options Options) *Formatter {
	hooks := make(map[Kind]Hook, len(options.Hooks))
	for kind, hook := range options.Hooks {
		hooks[kind] = hook
	}
	parallelism := options.Parallelism
	if parallelism <= 0 {
		parallelism = 4
	}
	return &Formatter{hooks: hooks, parallelism: parallelism}
}

var defaultFormatter = New(Options{})

// Clean performs deterministic formatting with no hooks or external calls.
// It is suitable for a latency-sensitive text-to-speech path.
func Clean(input string) string {
	output, _ := defaultFormatter.Format(context.Background(), input)
	return output
}

// Format removes presentation syntax, runs optional hooks on independent
// blocks, and assembles the results in source order. If a hook fails, no
// partial output is returned. The error names the first failing block in
// source order, regardless of hook completion order.
func (f *Formatter) Format(ctx context.Context, input string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(input) == "" {
		return "", nil
	}
	// Most conversational replies have no markup. Leave that prose intact
	// without constructing a Markdown parser or allocating an AST.
	if strings.IndexAny(input, "<&\r\n\t#*_-+`~[!|>\\") < 0 && !startsOrderedList(input) {
		return strings.TrimSpace(input), nil
	}
	source := []byte(input)
	document := goldmark.New(goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID())).Parser().Parse(text.NewReader(source))
	var parts []part
	collectBlocks(document, source, &parts)
	results := make([]string, len(parts))
	type task struct{ index int }
	jobs := make(chan task)
	var workers sync.WaitGroup
	var errs = make([]error, len(parts))
	count := 0
	for _, p := range parts {
		if p.block != nil && f.hooks[p.block.Kind] != nil {
			count++
		}
	}
	workerCount := min(f.parallelism, count)
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				block := *parts[job.index].block
				if err := ctx.Err(); err != nil {
					errs[job.index] = err
					continue
				}
				results[job.index], errs[job.index] = f.hooks[block.Kind](ctx, block)
			}
		}()
	}
	for i, p := range parts {
		if p.block == nil || f.hooks[p.block.Kind] == nil {
			results[i] = p.text
			continue
		}
		jobs <- task{index: i}
	}
	close(jobs)
	workers.Wait()
	for i, err := range errs {
		if err != nil {
			return "", fmt.Errorf("speechtext: %s block %d: %w", parts[i].block.Kind, i+1, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	for i := range results {
		results[i] = normalize(results[i])
	}
	return joinSentences(results), nil
}

type part struct {
	text  string
	block *Block
}

func collectBlocks(parent ast.Node, source []byte, parts *[]part) {
	for node := parent.FirstChild(); node != nil; node = node.NextSibling() {
		switch n := node.(type) {
		case *ast.FencedCodeBlock:
			*parts = append(*parts, part{block: &Block{Kind: KindCode, Content: lines(n.Lines(), source), Language: string(n.Language(source))}})
		case *ast.CodeBlock:
			*parts = append(*parts, part{block: &Block{Kind: KindCode, Content: lines(n.Lines(), source)}})
		case *ast.HTMLBlock:
			raw := lines(n.Lines(), source)
			if n.HasClosure() {
				raw += string(n.ClosureLine.Value(source))
			}
			*parts = append(*parts, part{text: htmlText(raw), block: &Block{Kind: KindHTML, Content: raw}})
		case *ast.Heading:
			value := normalize(inlineText(n, source))
			if value != "" && !hasEndPunctuation(value) {
				value += ":"
			}
			*parts = append(*parts, part{text: value})
		case *ast.Paragraph, *ast.TextBlock:
			*parts = append(*parts, part{text: inlineText(n, source)})
		case *extast.TableRow, *extast.TableHeader:
			var cells []string
			for cell := n.FirstChild(); cell != nil; cell = cell.NextSibling() {
				if value := normalize(inlineText(cell, source)); value != "" {
					cells = append(cells, value)
				}
			}
			*parts = append(*parts, part{text: strings.Join(cells, ", ")})
		case *ast.ThematicBreak:
			// A visual divider contributes no spoken words.
		default:
			collectBlocks(n, source, parts)
		}
	}
}

func lines(segments *text.Segments, source []byte) string {
	var out strings.Builder
	for i := range segments.Len() {
		segment := segments.At(i)
		out.Write(segment.Value(source))
	}
	return out.String()
}

func inlineText(parent ast.Node, source []byte) string {
	var out strings.Builder
	hiddenTag := ""
	var visit func(ast.Node)
	visit = func(node ast.Node) {
		switch n := node.(type) {
		case *ast.Text:
			if hiddenTag == "" {
				out.Write(n.Segment.Value(source))
			}
			if hiddenTag == "" && (n.SoftLineBreak() || n.HardLineBreak()) {
				out.WriteByte(' ')
			}
		case *ast.String:
			if hiddenTag == "" {
				out.Write(n.Value)
			}
		case *ast.AutoLink:
			if hiddenTag == "" {
				out.Write(n.Label(source))
			}
		case *ast.RawHTML:
			var raw strings.Builder
			for i := range n.Segments.Len() {
				segment := n.Segments.At(i)
				raw.Write(segment.Value(source))
			}
			markup := strings.TrimSpace(raw.String())
			lower := strings.ToLower(markup)
			if hiddenTag != "" {
				if strings.Contains(lower, "</"+hiddenTag) {
					hiddenTag = ""
				}
				return
			}
			for _, tag := range []string{"script", "style", "svg", "template", "noscript", "pre"} {
				if startsHTMLTag(lower, tag) && !strings.Contains(lower, "</"+tag) {
					hiddenTag = tag
					return
				}
			}
			if strings.HasPrefix(strings.ToLower(markup), "<br") {
				out.WriteByte(' ')
			} else {
				out.WriteString(htmlText(markup))
			}
		default:
			if hiddenTag == "" {
				for child := node.FirstChild(); child != nil; child = child.NextSibling() {
					visit(child)
				}
			}
		}
	}
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		visit(child)
	}
	return out.String()
}

func startsHTMLTag(markup, tag string) bool {
	opening := "<" + tag
	if !strings.HasPrefix(markup, opening) || len(markup) == len(opening) {
		return false
	}
	switch markup[len(opening)] {
	case ' ', '\t', '\n', '>', '/':
		return true
	}
	return false
}

func startsOrderedList(input string) bool {
	input = strings.TrimLeft(input, " ")
	i := 0
	for i < len(input) && input[i] >= '0' && input[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(input) && (input[i] == '.' || input[i] == ')') && (input[i+1] == ' ' || input[i+1] == '\t')
}

func htmlText(raw string) string {
	contextNode := &xhtml.Node{Type: xhtml.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := xhtml.ParseFragment(strings.NewReader(raw), contextNode)
	if err != nil {
		return ""
	}
	var out strings.Builder
	var visit func(*xhtml.Node)
	visit = func(node *xhtml.Node) {
		if node.Type == xhtml.TextNode {
			out.WriteString(node.Data)
			return
		}
		if node.Type == xhtml.ElementNode {
			switch node.Data {
			case "script", "style", "svg", "template", "noscript", "head", "pre":
				return
			case "br":
				out.WriteByte('\n')
				return
			}
		}
		block := node.Type == xhtml.ElementNode && htmlBlockElement(node.Data)
		if block {
			out.WriteByte('\n')
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
		if block {
			out.WriteByte('\n')
		}
	}
	for _, node := range nodes {
		visit(node)
	}
	var pieces []string
	for line := range strings.SplitSeq(out.String(), "\n") {
		if line = normalize(line); line != "" {
			pieces = append(pieces, line)
		}
	}
	return joinSentences(pieces)
}

func htmlBlockElement(name string) bool {
	switch name {
	case "article", "blockquote", "dd", "div", "dl", "dt", "h1", "h2", "h3", "h4", "h5", "h6", "li", "main", "ol", "p", "section", "table", "td", "th", "tr", "ul":
		return true
	}
	return false
}

func normalize(s string) string {
	s = stdhtml.UnescapeString(s)
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

func hasEndPunctuation(s string) bool {
	return strings.ContainsRune(".!?:;", rune(s[len(s)-1]))
}

func joinSentences(parts []string) string {
	var out strings.Builder
	for _, value := range parts {
		value = normalize(value)
		if value == "" {
			continue
		}
		if out.Len() != 0 {
			previous := out.String()
			if !hasEndPunctuation(previous) {
				out.WriteByte('.')
			}
			out.WriteByte(' ')
		}
		out.WriteString(value)
	}
	return out.String()
}
