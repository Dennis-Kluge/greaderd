package greader

import (
	"fmt"
	"testing"
)

// nnwItemID formats an ID the way NetNewsWire's itemIDParameter does.
func nnwItemID(id int64) string {
	return fmt.Sprintf("tag:google.com,2005:reader/item/%.16x", id)
}

func TestItemIDRoundTrip(t *testing.T) {
	for _, id := range []int64{1, 42, 1 << 40} {
		if got := longItemID(id); got != nnwItemID(id) {
			t.Errorf("longItemID(%d) = %q, want %q", id, got, nnwItemID(id))
		}
		if got, err := parseItemID(longItemID(id)); err != nil || got != id {
			t.Errorf("parseItemID(longItemID(%d)) = %d, %v", id, got, err)
		}
	}
}

func TestParseItemID(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "42", want: 42},
		{in: "tag:google.com,2005:reader/item/000000000000002a", want: 42},
		{in: "tag:google.com,2005:reader/item/2a", want: 42},
		{in: "tag:google.com,2005:reader/item/xyz", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseItemID(tt.in)
		if (err != nil) != tt.wantErr || (!tt.wantErr && got != tt.want) {
			t.Errorf("parseItemID(%q) = %d, %v", tt.in, got, err)
		}
	}
}

func TestParseLabel(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"user/-/label/Tech", "Tech", true},
		{"user/1234/label/Two Words", "Two Words", true},
		{"user/-/label/", "", false},
		{"user/-/state/com.google/starred", "", false},
		{"feed/1", "", false},
		{"label/Tech", "", false},
	}
	for _, tt := range tests {
		got, ok := parseLabel(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("parseLabel(%q) = %q, %v", tt.in, got, ok)
		}
	}
}

func TestParseState(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"user/-/state/com.google/read", stateRead},
		{"user/1/state/com.google/starred", stateStarred},
		{"user/-/state/com.google/reading-list", stateReadingList},
		{"user/-/state/org.freshrss/main", ""},
		{"user/-/label/read", ""},
		{"read", ""},
	}
	for _, tt := range tests {
		if got := parseState(tt.in); got != tt.want {
			t.Errorf("parseState(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
