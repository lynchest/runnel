package main

import (
	"strings"
	"testing"
)

func TestHTMLToMarkdownRemovesNoiseAndPreservesStructure(t *testing.T) {
	html := `<!doctype html><html><head><style>.x{}</style><script>bad()</script></head>
	<body><nav>Menu</nav><main><h1>Hello &amp; world</h1><p>Useful <strong>content</strong>.</p>
	<ul><li>One</li><li>Two</li></ul><pre>  keep
    indent</pre></main><footer>Noise</footer></body></html>`

	got, err := htmlToMarkdown([]byte(html), "text/html; charset=utf-8")
	if err != nil {
		t.Fatal(err)
	}
	want := "# Hello & world\n\nUseful content.\n- One\n- Two\n\n```\n  keep\n    indent\n```\n"
	if string(got) != want {
		t.Fatalf("markdown = %q, want %q", got, want)
	}
	for _, noise := range []string{"bad()", "Menu", "Noise"} {
		if strings.Contains(string(got), noise) {
			t.Fatalf("markdown contains skipped content %q", noise)
		}
	}
}
