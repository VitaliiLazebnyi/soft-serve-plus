package database

import (
	"fmt"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/matryer/is"
)

// Lists shown to users come back in a stable order. Rows are inserted out of
// order and then updated, which on Postgres moves an updated row to the end
// of a plain table scan.
func TestUserVisibleListsAreOrdered(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	rs, us, cs, ts, ls := &repoStore{}, &userStore{}, &collabStore{}, &accessTokenStore{}, &lfsStore{}

	for _, name := range []string{"c", "a", "b"} {
		is.NoErr(rs.CreateRepo(ctx, dbx, name, 100, name, "", false, false, false, false))
	}
	is.NoErr(rs.SetRepoDescriptionByName(ctx, dbx, "repo7", "moved"))
	is.NoErr(rs.SetRepoDescriptionByName(ctx, dbx, "a", "moved"))

	repos, err := rs.GetAllRepos(ctx, dbx)
	is.NoErr(err)
	userRepos, err := rs.GetUserRepos(ctx, dbx, 100)
	is.NoErr(err)
	for _, list := range [][]string{repoNames(repos), repoNames(userRepos)} {
		is.Equal(list, []string{"a", "b", "c", "repo7", "repo8"})
	}

	for _, name := range []string{"zed", "amy"} {
		is.NoErr(us.CreateUser(ctx, dbx, name, false, nil))
	}
	is.NoErr(us.SetAdminByUsername(ctx, dbx, "amy", true))
	users, err := us.GetAllUsers(ctx, dbx)
	is.NoErr(err)
	var usernames []string
	for _, u := range users {
		usernames = append(usernames, u.Username)
	}
	is.Equal(usernames, []string{"admin", "amy", "owner", "zed"})

	for _, name := range []string{"zed", "owner", "amy"} {
		is.NoErr(cs.AddCollabByUsernameAndRepo(ctx, dbx, name, "repo7", access.ReadOnlyAccess))
	}
	_, err = dbx.ExecContext(ctx, dbx.Rebind(`UPDATE collabs SET access_level = ? WHERE user_id = 100;`), access.ReadWriteAccess)
	is.NoErr(err)
	collabs, err := cs.ListCollabsByRepo(ctx, dbx, "repo7")
	is.NoErr(err)
	is.Equal(len(collabs), 3)
	for i := 1; i < len(collabs); i++ {
		is.True(collabs[i-1].ID < collabs[i].ID)
	}
	collabUsers, err := cs.ListCollabsByRepoAsUsers(ctx, dbx, "repo7")
	is.NoErr(err)
	usernames = nil
	for _, u := range collabUsers {
		usernames = append(usernames, u.Username)
	}
	is.Equal(usernames, []string{"amy", "owner", "zed"})

	var tokenIDs []int64
	for i := range 3 {
		tok, err := ts.CreateAccessToken(ctx, dbx, fmt.Sprintf("t%d", i), 100, fmt.Sprintf("hash%d", i), time.Time{})
		is.NoErr(err)
		tokenIDs = append(tokenIDs, tok.ID)
	}
	_, err = dbx.ExecContext(ctx, dbx.Rebind(`UPDATE access_tokens SET name = ? WHERE id = ?;`), "renamed", tokenIDs[0])
	is.NoErr(err)
	tokens, err := ts.GetAccessTokensByUserID(ctx, dbx, 100)
	is.NoErr(err)
	var gotTokenIDs []int64
	for _, tok := range tokens {
		gotTokenIDs = append(gotTokenIDs, tok.ID)
	}
	is.Equal(gotTokenIDs, tokenIDs)

	for _, oid := range []string{"oid1", "oid2", "oid3"} {
		is.NoErr(ls.CreateLFSObject(ctx, dbx, 7, oid, 1))
	}
	is.NoErr(ls.UpdateLFSObjectSize(ctx, dbx, 7, "oid1", 2))
	objs, err := ls.GetLFSObjects(ctx, dbx, 7)
	is.NoErr(err)
	is.Equal(lfsOids(objs), []string{"oid1", "oid2", "oid3"})
}

func repoNames(repos []models.Repo) []string {
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.Name
	}
	return names
}

func lfsOids(objs []models.LFSObject) []string {
	oids := make([]string, len(objs))
	for i, o := range objs {
		oids[i] = o.Oid
	}
	return oids
}
