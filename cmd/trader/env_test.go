package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDotenvDoesNotEvaluateShellOrOverwriteEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("# comment\nexport TRADER_TEST_A='$(touch /tmp/pwned)'\nTRADER_TEST_B=from-file\n"), 0o600)
	t.Setenv("TRADER_TEST_B", "from-process")
	os.Unsetenv("TRADER_TEST_A")
	t.Cleanup(func() { os.Unsetenv("TRADER_TEST_A") })
	if err := loadEnv(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TRADER_TEST_A") != "$(touch /tmp/pwned)" || os.Getenv("TRADER_TEST_B") != "from-process" {
		t.Fatal("dotenv evaluated shell code or overrode the process environment")
	}
	os.WriteFile(path, []byte("not an assignment\n"), 0o600)
	if err := loadEnv(path); err == nil {
		t.Fatal("malformed dotenv accepted")
	}
	os.WriteFile(path, []byte("TRADER_TEST_C=testnet\nTRADER_TEST_C=live\n"), 0o600)
	if err := loadEnv(path); err == nil || !strings.Contains(err.Error(), "TRADER_TEST_C is set twice") {
		t.Fatalf("a duplicate key must be rejected, as Docker Compose would read the other one: %v", err)
	}
	os.Unsetenv("TRADER_TEST_D")
	t.Cleanup(func() { os.Unsetenv("TRADER_TEST_D") })
	os.WriteFile(path, []byte("TRADER_TEST_D=live # real money\n"), 0o600)
	if err := loadEnv(path); err != nil || os.Getenv("TRADER_TEST_D") != "live" {
		t.Fatalf("inline comments must be dropped like Docker Compose does: %q %v", os.Getenv("TRADER_TEST_D"), err)
	}
	os.Unsetenv("TRADER_TEST_D")
	os.WriteFile(path, []byte(`TRADER_TEST_D="live" # real money`+"\n"), 0o600)
	if err := loadEnv(path); err != nil || os.Getenv("TRADER_TEST_D") != "live" {
		t.Fatalf("a quoted value followed by a comment: %q %v", os.Getenv("TRADER_TEST_D"), err)
	}
}
