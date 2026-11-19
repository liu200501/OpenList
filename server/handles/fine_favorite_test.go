package handles

import (
	"reflect"
	"testing"
)

func TestParseFineFavoriteNames(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "basic",
			input: "a\nb\nc",
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "with blanks and CRLF",
			input: " a \r\n\r\nb\r\nc ",
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "dedupe",
			input: "a\na\nb",
			want:  []string{"a", "b"},
		},
		{
			name:  "skip invalid",
			input: "a\n/b\n.\n..\n\nvalid name",
			want:  []string{"a", "valid name"},
		},
		{
			name:  "empty",
			input: "   \n\n",
			want:  []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFineFavoriteNames(tt.input)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseFineFavoriteNames(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeFineFavMount(t *testing.T) {
	tests := map[string]string{
		"/收藏":    "/收藏",
		"/收藏/":   "/收藏",
		"收藏":     "/收藏",
		"/收藏//a": "/收藏/a",
		"//":     "/",
	}
	for in, want := range tests {
		if got := normalizeFineFavMount(in); got != want {
			t.Errorf("normalizeFineFavMount(%q) = %q, want %q", in, got, want)
		}
	}
}
