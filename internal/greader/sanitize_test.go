package greader

import (
	"strings"
	"testing"
)

func TestHTMLPolicy(t *testing.T) {
	p := newHTMLPolicy()
	tests := []struct {
		name    string
		in      string
		want    []string // substrings that must survive
		notWant []string // substrings that must be gone
	}{
		{
			name: "formatting, links and images survive",
			in:   `<p>Hello <strong>world</strong> <a href="https://example.com/">link</a> <img src="https://example.com/a.png" alt="a" loading="lazy"></p>`,
			want: []string{"<strong>world</strong>", `href="https://example.com/"`, `src="https://example.com/a.png"`, `loading="lazy"`},
		},
		{
			name:    "scripts and styles are removed",
			in:      `<p>ok</p><script>alert(1)</script><style>body{display:none}</style>`,
			want:    []string{"<p>ok</p>"},
			notWant: []string{"script", "alert", "style", "display"},
		},
		{
			name:    "event handlers are removed",
			in:      `<img src="https://example.com/a.png" onerror="alert(1)">`,
			want:    []string{`src="https://example.com/a.png"`},
			notWant: []string{"onerror", "alert"},
		},
		{
			name:    "javascript URLs are removed",
			in:      `<a href="javascript:alert(1)">x</a>`,
			notWant: []string{"javascript", "alert"},
		},
		{
			name: "video embeds survive",
			in:   `<iframe src="https://www.youtube-nocookie.com/embed/abc" width="560" height="315" allowfullscreen></iframe>`,
			want: []string{`src="https://www.youtube-nocookie.com/embed/abc"`, `width="560"`, "allowfullscreen"},
		},
		{
			name:    "other iframes lose their source",
			in:      `<iframe src="https://evil.example/frame"></iframe>`,
			notWant: []string{"evil.example"},
		},
		{
			name:    "forms are removed",
			in:      `<form action="https://evil.example/"><input name="password"></form>`,
			notWant: []string{"form", "input", "evil.example"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Sanitize(tt.in)
			for _, s := range tt.want {
				if !strings.Contains(got, s) {
					t.Errorf("Sanitize(%q) = %q, missing %q", tt.in, got, s)
				}
			}
			for _, s := range tt.notWant {
				if strings.Contains(got, s) {
					t.Errorf("Sanitize(%q) = %q, still contains %q", tt.in, got, s)
				}
			}
		})
	}
}
