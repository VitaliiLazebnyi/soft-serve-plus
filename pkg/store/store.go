package store

// Store is an interface for managing repositories, users, and settings.
//
// An update or delete of one row by name or ID that matches no row returns
// db.ErrRecordNotFound, so a caller that raced a rename or a delete is told.
type Store interface {
	RepositoryStore
	UserStore
	CollaboratorStore
	SettingStore
	LFSStore
	AccessTokenStore
	WebhookStore
}
