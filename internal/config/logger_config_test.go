package config

import "testing"

func TestLoggerControlConfigurationAndSecretPrecedence(t *testing.T) {
	values := map[string]string{
		"APP_SECRET_FILE": "/secrets/application",
		"FLIXER_URL":      "https://flixer.example/",
	}
	get := func(key string) string { return values[key] }
	cfg, err := FromEnv(get, "test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SecretFile != "/secrets/application" || cfg.FlixerURL != "https://flixer.example" {
		t.Fatalf("unexpected control settings: %+v", cfg)
	}
	values["STREAM_TOKEN_SECRET_FILE"] = "/secrets/legacy"
	cfg, err = FromEnv(get, "test")
	if err != nil || cfg.SecretFile != "/secrets/legacy" {
		t.Fatalf("legacy secret precedence: %+v, %v", cfg, err)
	}
}
