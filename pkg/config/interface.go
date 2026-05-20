package config

import "context"

// AdminStore defines the set of administrative operations on the configuration database.
// Both the standard Firestore/local ConfigStore and the decoupled REST APIConfigStore implement this interface.
type AdminStore interface {
	GetAllKeys(ctx context.Context) ([]APIKey, error)
	GetAllClients(ctx context.Context) ([]Client, error)
	GetAllApps(ctx context.Context) ([]App, error)
	LookupApp(appID string) (App, bool)
	SaveKey(ctx context.Context, key APIKey) error
	RevokeKey(ctx context.Context, hash string) error
	GetAllRules(ctx context.Context) ([]RoutingRule, error)
	GetAllModels(ctx context.Context) ([]ModelConfig, error)
	SaveRule(ctx context.Context, rule RoutingRule) error
	DeleteRule(ctx context.Context, id string) error
	GetAllHeaders(ctx context.Context) ([]CustomHeader, error)
	SaveHeader(ctx context.Context, header CustomHeader) error
	DeleteHeader(ctx context.Context, id string) error
	SaveModel(ctx context.Context, model ModelConfig) error
	DeleteModel(ctx context.Context, id string) error
	SaveApp(ctx context.Context, app App) error
	DeleteApp(ctx context.Context, id string) error
	SaveClient(ctx context.Context, client Client) error
	DeleteClient(ctx context.Context, id string) error
	GetQueueStatus(ctx context.Context) ([]QueueSnapshotItem, error)
}
