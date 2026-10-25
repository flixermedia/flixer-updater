package config

import "testing"

func TestBackupDirectoryDefaultsAndOverrides(t *testing.T) {
	values := map[string]string{"STREAM_TOKEN_SECRET_FILE": "/secrets/token"}
	get := func(key string) string { return values[key] }
	cfg, err := FromEnv(get, "test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigDir != "/var/lib/flixer/config" || cfg.SecretsDir != "/run/flixer-secrets" || cfg.MetadataDir != "/data/metadata" || cfg.BackupDir != "/backups" || cfg.PassphraseFile != "/backups/.passphrase" {
		t.Fatalf("defaults: %+v", cfg)
	}
	values["CONFIG_DIR"] = "/custom/config"
	values["SECRETS_DIR"] = "/custom/secrets"
	values["METADATA_DIR"] = "/custom/metadata"
	values["BACKUP_DIR"] = "/custom/backups"
	if cfg, _ = FromEnv(get, "test"); cfg.PassphraseFile != "/custom/backups/.passphrase" {
		t.Fatalf("passphrase default follows BACKUP_DIR: %s", cfg.PassphraseFile)
	}
	values["BACKUP_PASSPHRASE_FILE"] = "/custom/pass"
	cfg, err = FromEnv(get, "test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigDir != "/custom/config" || cfg.SecretsDir != "/custom/secrets" || cfg.MetadataDir != "/custom/metadata" || cfg.BackupDir != "/custom/backups" || cfg.PassphraseFile != "/custom/pass" {
		t.Fatalf("overrides: %+v", cfg)
	}
}
