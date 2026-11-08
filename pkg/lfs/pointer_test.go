package lfs

import (
	"errors"
	"path"
	"strconv"
	"testing"
)

func TestReadPointer(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    Pointer
		// wantErr is the sentinel error the pointer is rejected with.
		wantErr error
		// wantNumErr says the pointer is rejected with a *strconv.NumError.
		wantNumErr bool
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
			wantNumErr: true,
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ReadPointerFromBuffer([]byte(tc.content))
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ReadPointerFromBuffer() error = %v, want %v", err, tc.wantErr)
				}
				return
			case tc.wantNumErr:
				var numErr *strconv.NumError
				if !errors.As(err, &numErr) {
					t.Fatalf("ReadPointerFromBuffer() error = %v (%T), want a *strconv.NumError", err, err)
				}
				return
			case err != nil:
				t.Fatalf("ReadPointerFromBuffer() error = %v, want nil", err)
			}

			if !p.IsValid() {
				t.Errorf("Expected a valid pointer")
			}
			if path.Join(p.Oid[:2], p.Oid[2:4], p.Oid) != p.RelativePath() {
				t.Errorf("Expected a valid relative path")
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
