package services

import (
	"oneimg/backend/config"
	"oneimg/backend/database"
	"path/filepath"
)

type Services struct {
	Config   *config.Config
	Accounts *AccountService
	Passkeys *PasskeyService
	Tokens   *TokenService
	Tags     *TagService
	Images   *ImageService
	Users    *UserService
	Avatars  *AvatarService
	Settings *SettingsService
	Stats    *StatsService
	Storage  *StorageService
	Uploads  *UploadService
	Imports  *RemoteImporter
}

func New(db *database.Database, cfg *config.Config) *Services {
	result := &Services{
		Config: cfg,
		Tokens: NewTokenService(db.DB, cfg.ConfigSecret),
	}
	result.Accounts = NewAccountService(db.DB)
	result.Accounts.recoveryPath = filepath.Join(cfg.StateDirectory(), ".admin_recovery_code")
	result.Passkeys = NewPasskeyService(db.DB, result.Accounts)
	result.Tags = NewTagService(db.DB)
	result.Images = NewImageService(db.DB)
	result.Avatars = NewAvatarService(db.DB, filepath.Join(cfg.StateDirectory(), "avatars"))
	result.Users = NewUserService(db.DB)
	result.Users.avatars = result.Avatars
	result.Settings = NewSettingsService(db.DB)
	result.Stats = NewStatsService(db.DB)
	result.Storage = NewStorageService(db.DB)
	result.Uploads = NewUploadService(db.DB)
	result.Imports = NewRemoteImporter(ImportConfig{})
	return result
}
