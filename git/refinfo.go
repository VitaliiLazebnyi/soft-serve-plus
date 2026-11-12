package git

import (
	"strings"
	"time"

	"github.com/aymanbagabas/git-module"
)

// RefInfo is a reference with the details of the commit it points to, as
// listed by Repository.RefInfos.
type RefInfo struct {
	*Reference
	// CommitID is the commit the reference points to, after peeling an
	// annotated tag. It is empty when the reference does not point to a
	// commit, such as a tag of a tree.
	CommitID string
	// AuthorWhen and CommitterWhen are the commit's author and committer
	// dates. They are zero when CommitID is empty.
	AuthorWhen    time.Time
	CommitterWhen time.Time
	// TagSubject is the subject of an annotated tag's message. It is empty
	// for branches and lightweight tags.
	TagSubject string
}

// refInfoFields are the for-each-ref fields RefInfos reads, in order. The
// asterisk fields describe the object an annotated tag points to.
var refInfoFields = []string{
	"%(refname)",
	"%(objecttype)",
	"%(objectname)",
	"%(authordate:iso-strict)",
	"%(committerdate:iso-strict)",
	"%(*objecttype)",
	"%(*objectname)",
	"%(*authordate:iso-strict)",
	"%(*committerdate:iso-strict)",
	"%(contents:subject)",
}

// RefInfos returns the references under prefix, such as RefsTags, with the
// commit each one points to. It runs a single git for-each-ref, however many
// references there are.
func (r *Repository) RefInfos(prefix string) ([]*RefInfo, error) {
	out, err := git.NewCommandWithContext(r.Context(), "for-each-ref",
		"--format="+strings.Join(refInfoFields, "%00"), prefix).RunInDir(r.Path)
	if err != nil {
		return nil, err
	}
	var infos []*RefInfo
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != len(refInfoFields) {
			continue
		}
		name, typ, id, author, committer := f[0], f[1], f[2], f[3], f[4]
		info := &RefInfo{
			Reference: &Reference{
				Reference: &git.Reference{ID: id, Refspec: name},
				path:      r.Path,
			},
		}
		if typ == "tag" {
			info.TagSubject = f[9]
			typ, id, author, committer = f[5], f[6], f[7], f[8]
		}
		if typ == "commit" {
			info.CommitID = id
			info.AuthorWhen, _ = time.Parse(time.RFC3339, author)
			info.CommitterWhen, _ = time.Parse(time.RFC3339, committer)
		}
		infos = append(infos, info)
	}
	return infos, nil
}
