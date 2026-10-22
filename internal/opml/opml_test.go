package opml

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []Subscription
	}{
		{
			name: "flat",
			doc: `<opml version="2.0"><body>
				<outline text="A" xmlUrl="https://a.example/feed" htmlUrl="https://a.example/"/>
			</body></opml>`,
			want: []Subscription{{Title: "A", FeedURL: "https://a.example/feed", SiteURL: "https://a.example/"}},
		},
		{
			name: "title attribute wins over text",
			doc:  `<opml><body><outline text="text" title="title" xmlUrl="https://a.example/feed"/></body></opml>`,
			want: []Subscription{{Title: "title", FeedURL: "https://a.example/feed"}},
		},
		{
			name: "nested folders collapse to the top level",
			doc: `<opml><body>
				<outline text="Tech">
					<outline text="B" xmlUrl="https://b.example/feed"/>
					<outline text="Go">
						<outline text="C" xmlUrl="https://c.example/feed"/>
					</outline>
				</outline>
			</body></opml>`,
			want: []Subscription{
				{Title: "B", FeedURL: "https://b.example/feed", Folder: "Tech"},
				{Title: "C", FeedURL: "https://c.example/feed", Folder: "Tech"},
			},
		},
		{
			name: "empty body",
			doc:  `<opml><body/></opml>`,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(tt.doc))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	if _, err := Parse(strings.NewReader("not xml")); err == nil {
		t.Error("Parse of invalid input succeeded")
	}
}

func TestWriteRoundTrip(t *testing.T) {
	subs := []Subscription{
		{Title: "A", FeedURL: "https://a.example/feed", SiteURL: "https://a.example/", Folder: "Tech"},
		{Title: "B", FeedURL: "https://b.example/feed", Folder: "News"},
		{Title: "C", FeedURL: "https://c.example/feed", Folder: "Tech"},
		{Title: "D", FeedURL: "https://d.example/feed"},
	}
	var buf bytes.Buffer
	if err := Write(&buf, "test", subs); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Parse(&buf)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Write groups by folder, so C follows A.
	want := []Subscription{subs[0], subs[2], subs[1], subs[3]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}
