package database

// The reads name the columns they scan rather than SELECT * or RETURNING *.
// sqlx refuses a row with a column the destination struct lacks, so with *
// a migration that adds a column breaks every read of its table until the
// model has the field too. Each list is a table's columns that its model in
// pkg/db/models scans, in the model's field order; TestColumnListsMatchModels
// checks that. The qualified lists, the same with the table name in front,
// are for joins.
const (
	accessTokenColumns      = "id, name, user_id, token, expires_at, created_at, updated_at" //nolint:gosec // G101: column names, not a credential
	collabColumns           = "id, repo_id, user_id, access_level, created_at, updated_at"
	collabColumnsQualified  = "collabs.id, collabs.repo_id, collabs.user_id, collabs.access_level, collabs.created_at, collabs.updated_at"
	lfsObjectColumns        = "id, oid, size, repo_id, created_at, updated_at"
	lfsLockColumns          = "id, path, user_id, repo_id, refname, created_at, updated_at"
	lfsLockColumnsQualified = "lfs_locks.id, lfs_locks.path, lfs_locks.user_id, lfs_locks.repo_id, lfs_locks.refname, lfs_locks.created_at, lfs_locks.updated_at"
	repoColumns             = "id, name, project_name, description, private, mirror, hidden, user_id, created_at, updated_at, anonymous, mirror_synced_at, mirror_error"
	repoColumnsQualified    = "repos.id, repos.name, repos.project_name, repos.description, repos.private, repos.mirror, repos.hidden, repos.user_id, repos.created_at, repos.updated_at, repos.anonymous, repos.mirror_synced_at, repos.mirror_error"
	userColumns             = "id, username, admin, password, created_at, updated_at"
	userColumnsQualified    = "users.id, users.username, users.admin, users.password, users.created_at, users.updated_at"
	webhookColumns          = "id, repo_id, url, secret, content_type, active, created_at, updated_at"
	webhookColumnsQualified = "webhooks.id, webhooks.repo_id, webhooks.url, webhooks.secret, webhooks.content_type, webhooks.active, webhooks.created_at, webhooks.updated_at"
	webhookEventColumns     = "id, webhook_id, event, created_at"
	webhookDeliveryColumns  = "id, webhook_id, event, request_url, request_method, request_error, request_headers, request_body, response_status, response_headers, response_body, created_at, seq"
)
