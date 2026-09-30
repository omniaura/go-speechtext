# go-speechtext

Deterministic Markdown and HTML cleanup for text-to-speech in Go. It removes
formatting markers while keeping visible words, headings, list items, link
labels, and HTML text. Fenced, indented, and HTML preformatted code blocks are
omitted by default.
It makes no network or model calls.

```go
spoken := speechtext.Clean("## Summary\n**Ready** to ship.")
// "Summary: Ready to ship."
```

Use a hook when an application can describe special content more usefully:

```go
formatter := speechtext.New(speechtext.Options{
    Parallelism: 3,
    Hooks: map[speechtext.Kind]speechtext.Hook{
        speechtext.KindCode: func(ctx context.Context, block speechtext.Block) (string, error) {
            // block.Language and block.Content are the original code block.
            return describeCode(ctx, block.Language, block.Content)
        },
    },
})
spoken, err := formatter.Format(ctx, markdown)
```

Hooks for separate blocks may run in parallel; their results are assembled in
source order. Hooks must be concurrency-safe. A hook may return an empty string
to omit its block. `KindHTML` can override the built-in visible-text extraction
for HTML blocks. Hook errors return no partial transcript. This is a text
cleanup tool, not a semantic summarizer: it does not invent descriptions for
code, formulas, or structured data.

MIT licensed.
