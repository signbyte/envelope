package envelope

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

// boundForFileSecrets reads the configuration from the environment the way the service does at start.
func boundForFileSecrets(t *testing.T) *Configuration {
	t.Helper()

	c := NewConfiguration()
	v := viper.New()
	c.Bind("", v)
	if err := v.Unmarshal(c); err != nil {
		t.Fatal(err)
	}

	return c
}

func secretFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// The connection string carries the database role's password, so it may arrive
// as a file the secret store's agent writes (ENVELOPE_STORE_DSN_FILE).
func TestTheStoreAddressIsReadFromAFile(t *testing.T) {
	t.Setenv("ENVELOPE_STORE_DSN_FILE", secretFile(t, "postgres://envelope_public:pw@db:5432/fleet\n"))

	if got := boundForFileSecrets(t).StoreDSN; got != "postgres://envelope_public:pw@db:5432/fleet" {
		t.Fatalf("store DSN = %q, want the file's content", got)
	}
}

// An explicit environment value still wins over the file.
func TestTheStoreAddressFromTheEnvironmentWinsOverTheFile(t *testing.T) {
	t.Setenv("ENVELOPE_STORE_DSN_FILE", secretFile(t, "postgres://from-file"))
	t.Setenv("ENVELOPE_STORE_DSN", "postgres://from-env")

	if got := boundForFileSecrets(t).StoreDSN; got != "postgres://from-env" {
		t.Fatalf("store DSN = %q, want the environment value", got)
	}
}
