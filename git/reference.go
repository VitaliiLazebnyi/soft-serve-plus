package git

import (
	"strings"

	"github.com/aymanbagabas/git-module"
)

const (
	// HEAD represents the name of the HEAD reference.
	HEAD = "HEAD"
	// RefsHeads represents the prefix for branch references.
	RefsHeads = git.RefsHeads
	// RefsTags represents the prefix for tag references.
	RefsTags = git.RefsTags
)

// Reference is a reference of a repository, such as a branch or a tag, and
// the object it points to.
type Reference struct {
	// id is the ID of the object the reference points to.
	id string
	// refspec is the reference's full name, such as refs/heads/main.
	refspec string
}

// NewReference returns the reference named refspec, a full name such as
// refs/heads/main, that points to the object id.
func NewReference(id, refspec string) *Reference {
	return &Reference{id: id, refspec: refspec}
}

// ID returns the ID of the object the reference points to.
func (r *Reference) ID() string {
	return r.id
}

// ReferenceName is a Refspec wrapper.
type ReferenceName string

// String returns the reference name i.e. refs/heads/master.
func (r ReferenceName) String() string {
	return string(r)
}

// Short returns the short name of the reference i.e. master.
func (r ReferenceName) Short() string {
	return git.RefShortName(string(r))
}

// Name returns the reference name i.e. refs/heads/master.
func (r *Reference) Name() ReferenceName {
	return ReferenceName(r.refspec)
}

// IsBranch returns true if the reference is a branch.
func (r *Reference) IsBranch() bool {
	return strings.HasPrefix(r.refspec, git.RefsHeads)
}

// IsTag returns true if the reference is a tag.
func (r *Reference) IsTag() bool {
	return strings.HasPrefix(r.refspec, git.RefsTags)
}
