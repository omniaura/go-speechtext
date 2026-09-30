package speechtext

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCleanMarkdown(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{"plain", "The API costs $5 and runs in 3 ms.", "The API costs $5 and runs in 3 ms."},
		{"heading and emphasis", "## Summary\n**Ready** to _ship_.", "Summary: Ready to ship."},
		{"list", "To do:\n\n- Build the API\n- Test it", "To do: Build the API. Test it"},
		{"links", "See [the guide](https://example.com) and ![the chart](chart.png).", "See the guide and the chart."},
		{"quote", "> First point.\n> Second point.", "First point. Second point."},
		{"inline code", "Call `fmt.Println` and keep user_name as written.", "Call fmt.Println and keep user_name as written."},
		{"fenced code", "Before.\n\n```go\nfmt.Println(\"hidden\")\n```\n\nAfter.", "Before. After."},
		{"tilde fence", "Start.\n\n~~~json\n{\"secret\":1}\n~~~\n\nEnd.", "Start. End."},
		{"indented code", "Before.\n\n    doNotRead()\n\nAfter.", "Before. After."},
		{"code only", "```go\nfmt.Println(1)\n```", ""},
		{"table", "| Name | Value |\n| --- | --- |\n| Foo | 2 |", "Name, Value. Foo, 2"},
		{"empty", "  \n ", ""},
		{"ordinary punctuation", "A_B, 50%, R&D, and x^2.", "A_B, 50%, R&D, and x^2."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Clean(tc.input); got != tc.want {
				t.Fatalf("Clean(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestCleanHTML(t *testing.T) {
	cases := []struct{ input, want string }{
		{"<div><h2>News</h2><p>Ship <strong>today</strong> &amp; rest.</p></div>", "News. Ship today & rest."},
		{"Hello <em>friend</em><br>See you.", "Hello friend See you."},
		{"<p>Visible</p><script>alert('hidden')</script><style>.x{display:none}</style>", "Visible"},
		{"<!-- internal -->Visible &amp; useful", "Visible & useful"},
	}
	for _, tc := range cases {
		if got := Clean(tc.input); got != tc.want {
			t.Fatalf("Clean(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestHooksSelectionAndSourceOrder(t *testing.T) {
	var mu sync.Mutex
	var seen []Block
	formatter := New(Options{Hooks: map[Kind]Hook{
		KindCode: func(_ context.Context, b Block) (string, error) {
			mu.Lock()
			seen = append(seen, b)
			mu.Unlock()
			return "A Go example", nil
		},
		KindHTML: func(_ context.Context, b Block) (string, error) {
			return "An HTML panel", nil
		},
	}})
	input := "Intro.\n\n```go\nfmt.Println(1)\n```\n\n<div>Default text</div>\n\nEnd."
	got, err := formatter.Format(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Intro. A Go example. An HTML panel. End." {
		t.Fatalf("output = %q", got)
	}
	if len(seen) != 1 || seen[0].Kind != KindCode || seen[0].Language != "go" || !strings.Contains(seen[0].Content, "fmt.Println(1)") {
		t.Fatalf("code hook received %#v", seen)
	}
}

func TestHooksRunConcurrentlyAndKeepOrder(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	formatter := New(Options{Parallelism: 2, Hooks: map[Kind]Hook{
		KindCode: func(_ context.Context, b Block) (string, error) {
			started <- strings.TrimSpace(b.Content)
			<-release
			return strings.TrimSpace(b.Content), nil
		},
	}})
	var got string
	var err error
	done := make(chan struct{})
	go func() {
		got, err = formatter.Format(context.Background(), "```\nfirst\n```\n\n```\nsecond\n```")
		close(done)
	}()
	for range 2 {
		select {
		case <-started:
		case <-done:
			t.Fatal("hook returned before both independent blocks started")
		case <-time.After(2 * time.Second):
			t.Fatal("independent hooks did not start concurrently")
		}
	}
	close(release)
	<-done
	if err != nil || got != "first. second" {
		t.Fatalf("Format = %q, %v", got, err)
	}
}

func TestHookErrorReturnsNoPartialText(t *testing.T) {
	want := errors.New("description unavailable")
	formatter := New(Options{Hooks: map[Kind]Hook{
		KindCode: func(context.Context, Block) (string, error) { return "", want },
	}})
	got, err := formatter.Format(t.Context(), "Before.\n\n```\nx\n```\n\nAfter.")
	if got != "" || !errors.Is(err, want) {
		t.Fatalf("Format = %q, %v", got, err)
	}
}

func TestCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := New(Options{}).Format(ctx, "Hello")
	if got != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("Format = %q, %v", got, err)
	}
}
