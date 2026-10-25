package config

import "testing"

func TestBackupDirectoryDefaultsAndOverrides(t *testing.T) {
	values := map[string]string{"STREAM_TOKEN_SECRET_FILE": "/secrets/token", "BACKUP_DIR": "/custom/backups"}
	get := func(key string) string { return values[key] }
	cfg, err := FromEnv(get, "test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigDir != "/data/config" || cfg.SecretsDir != "/run/flixer-secrets" || cfg.CacheDir != "/data/cache" || cfg.BackupDir != "/data/backups" || cfg.PassphraseFile != "/data/backups/.passphrase" {
		t.Fatalf("defaults: %+v", cfg)
	}
	values["DATA_PATH"] = "/custom/data"
	values["SECRETS_DIR"] = "/custom/secrets"
	values["BACKUP_PASSPHRASE_FILE"] = "/custom/pass"
	cfg, err = FromEnv(get, "test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigDir != "/custom/data/config" || cfg.SecretsDir != "/custom/secrets" || cfg.CacheDir != "/custom/data/cache" || cfg.BackupDir != "/custom/data/backups" || cfg.PassphraseFile != "/custom/pass" {
		t.Fatalf("overrides: %+v", cfg)
	}
}
