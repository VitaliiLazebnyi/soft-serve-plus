package lfs

import (
	"errors"
	"path"
	"testing"
)

const (
	testOid  = "1234567890123456789012345678901234567890123456789012345678901234"
	testOid2 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

func TestReadPointer(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    Pointer
		// wantErr is the sentinel error the pointer is rejected with.
		wantErr error
	}{
		{
			name: "valid pointer",
			content: `version https://git-lfs.github.com/spec/v1
oid sha256:1234567890123456789012345678901234567890123456789012345678901234
size 1234
`,
			want: Pointer{
				Oid:  "1234567890123456789012345678901234567890123456789012345678901234",
				Size: 1234,
			},
		},
		{
			name: "invalid prefix",
			content: `version https://foobar/spec/v2
oid sha256:1234567890123456789012345678901234567890123456789012345678901234
size 1234
`,
			wantErr: ErrMissingPrefix,
		},
		{
			name: "invalid oid",
			content: `version https://git-lfs.github.com/spec/v1
oid sha256:&2345a78$012345678901234567890123456789012345678901234567890123
size 1234
`,
			wantErr: ErrInvalidOIDFormat,
		},
		{
			name: "invalid size",
			content: `version https://git-lfs.github.com/spec/v1
oid sha256:1234567890123456789012345678901234567890123456789012345678901234
size abc
`,
			wantErr: ErrInvalidSize,
		},
		{
			name: "invalid structure",
			content: `version https://git-lfs.github.com/spec/v1
`,
			wantErr: ErrInvalidStructure,
		},
		{
			name:    "empty pointer",
			wantErr: ErrMissingPrefix,
		},
		{
			name:    "no trailing newline",
			content: "version https://git-lfs.github.com/spec/v1\noid sha256:" + testOid + "\nsize 1234",
			want:    Pointer{Oid: testOid, Size: 1234},
		},
		{
			name:    "zero size",
			content: "version https://git-lfs.github.com/spec/v1\noid sha256:" + testOid + "\nsize 0\n",
			want:    Pointer{Oid: testOid, Size: 0},
		},
		{
			name: "extensions",
			content: "version https://git-lfs.github.com/spec/v1\n" +
				"ext-0-foo sha256:" + testOid2 + "\n" +
				"ext-1-bar_baz sha256:" + testOid2 + "\n" +
				"oid sha256:" + testOid + "\nsize 1234\n",
			want: Pointer{Oid: testOid, Size: 1234},
		},
		{
			name:    "version suffix",
			content: "version https://git-lfs.github.com/spec/v1garbage\noid sha256:" + testOid + "\nsize 1234\n",
			wantErr: ErrMissingPrefix,
		},
		{
			name:    "oid without key",
			content: "version https://git-lfs.github.com/spec/v1\n" + testOid + "\nsize 1234\n",
			wantErr: ErrInvalidStructure,
		},
		{
			name:    "oid without hash algorithm",
			content: "version https://git-lfs.github.com/spec/v1\noid " + testOid + "\nsize 1234\n",
			wantErr: ErrInvalidOIDFormat,
		},
		{
			name:    "size without key",
			content: "version https://git-lfs.github.com/spec/v1\noid sha256:" + testOid + "\n1234\n",
			wantErr: ErrInvalidStructure,
		},
		{
			name:    "size with plus sign",
			content: "version https://git-lfs.github.com/spec/v1\noid sha256:" + testOid + "\nsize +5\n",
			wantErr: ErrInvalidSize,
		},
		{
			name:    "negative size",
			content: "version https://git-lfs.github.com/spec/v1\noid sha256:" + testOid + "\nsize -5\n",
			wantErr: ErrInvalidSize,
		},
		{
			name:    "size overflow",
			content: "version https://git-lfs.github.com/spec/v1\noid sha256:" + testOid + "\nsize 99999999999999999999\n",
			wantErr: ErrInvalidSize,
		},
		{
			name:    "size with trailing text",
			content: "version https://git-lfs.github.com/spec/v1\noid sha256:" + testOid + "\nsize 5 bytes\n",
			wantErr: ErrInvalidSize,
		},
		{
			name:    "extra line",
			content: "version https://git-lfs.github.com/spec/v1\noid sha256:" + testOid + "\nsize 1234\nfoo bar\n",
			wantErr: ErrInvalidStructure,
		},
		{
			name:    "trailing blank line",
			content: "version https://git-lfs.github.com/spec/v1\noid sha256:" + testOid + "\nsize 1234\n\n",
			wantErr: ErrInvalidStructure,
		},
		{
			name:    "oid and size swapped",
			content: "version https://git-lfs.github.com/spec/v1\nsize 1234\noid sha256:" + testOid + "\n",
			wantErr: ErrInvalidStructure,
		},
		{
			name: "extension after oid",
			content: "version https://git-lfs.github.com/spec/v1\noid sha256:" + testOid + "\n" +
				"ext-0-foo sha256:" + testOid2 + "\nsize 1234\n",
			wantErr: ErrInvalidStructure,
		},
		{
			name: "extensions out of order",
			content: "version https://git-lfs.github.com/spec/v1\n" +
				"ext-1-foo sha256:" + testOid2 + "\n" +
				"ext-0-bar sha256:" + testOid2 + "\n" +
				"oid sha256:" + testOid + "\nsize 1234\n",
			wantErr: ErrInvalidStructure,
		},
		{
			name: "malformed extension",
			content: "version https://git-lfs.github.com/spec/v1\n" +
				"ext-0-foo sha256:xyz\n" +
				"oid sha256:" + testOid + "\nsize 1234\n",
			wantErr: ErrInvalidStructure,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ReadPointerFromBuffer([]byte(tc.content))
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ReadPointerFromBuffer() error = %v (%T), want %v", err, err, tc.wantErr)
				}
				return
			case err != nil:
				t.Fatalf("ReadPointerFromBuffer() error = %v, want nil", err)
			}

			if !p.IsValid() {
				t.Errorf("Expected a valid pointer")
				return
			}
			if path.Join(p.Oid[:2], p.Oid[2:4], p.Oid) != p.RelativePath() {
				t.Errorf("Expected a valid relative path")
				return
			}
			if p.Oid != tc.want.Oid {
				t.Errorf("ReadPointerFromBuffer() oid = %v, want %v", p.Oid, tc.want.Oid)
			}
			if p.Size != tc.want.Size {
				t.Errorf("ReadPointerFromBuffer() size = %v, want %v", p.Size, tc.want.Size)
			}
		})
	}
}
