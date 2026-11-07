package git

import (
	"testing"

	"github.com/matryer/is"
)

func TestParseAttr(t *testing.T) {
	cases := []struct {
		in   string
		want []Attribute
	}{
		{
			in: "org/example/MyClass.java\x00diff\x00java\x00",
			want: []Attribute{
				{
					Name:  "diff",
					Value: "java",
				},
			},
		},
		{
			in: "org/example/MyClass.java\x00crlf\x00unset\x00" +
				"org/example/MyClass.java\x00diff\x00java\x00" +
				"org/example/MyClass.java\x00myAttr\x00set\x00",
			want: []Attribute{
				{
					Name:  "crlf",
					Value: "unset",
				},
				{
					Name:  "diff",
					Value: "java",
				},
				{
					Name:  "myAttr",
					Value: "set",
				},
			},
		},
		{
			in: "README\x00caveat\x00unspecified\x00",
			want: []Attribute{
				{
					Name:  "caveat",
					Value: "unspecified",
				},
			},
		},
		{
			// git would C-quote this path without -z.
			in: "dir/café: x.txt\x00binary\x00set\x00",
			want: []Attribute{
				{
					Name:  "binary",
					Value: "set",
				},
			},
		},
		{
			in:   "",
			want: []Attribute{},
		},
	}

	is := is.New(t)
	for _, c := range cases {
		attrs := parseAttributes([]byte(c.in))
		if len(attrs) != len(c.want) {
			t.Fatalf("parseAttributes(%q) = %v, want %v", c.in, attrs, c.want)
		}

		is.Equal(attrs, c.want)
	}
}
