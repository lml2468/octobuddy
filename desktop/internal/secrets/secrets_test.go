package secrets

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lml2468/octobuddy/core/control/wire"
	"github.com/lml2468/octobuddy/desktop/internal/desktest"
)

// TestAccountRejectsInvalidBotID is the security regression: every
// callsite that derives the keyring/file account key MUST refuse a
// traversal-shaped id BEFORE it reaches the backend, so an attacker who
// controls a botID can't read or stomp another bot's namespace.
func TestAccountRejectsInvalidBotID(t *testing.T) {
	for _, bad := range []string{"../other", ".", "..", "with/slash", ""} {
		t.Run(bad, func(t *testing.T) {
			if _, err := account(bad, wire.SecretKindOcto); err == nil {
				t.Fatalf("account(%q) must reject, got nil error", bad)
			}
		})
	}
}

// TestSecretEnvNameNormalizesNonAlphanumerics: the env var convention
// must be stable so an operator setting `OCTOBUDDY_SECRET_<id>_<kind>=…`
// in CI gets read by Get(). Non-alpha-numeric chars are squashed to "_".
func TestSecretEnvNameNormalizesNonAlphanumerics(t *testing.T) {
	got := secretEnvName("my-bot.v2", wire.SecretKindOcto)
	want := "OCTOBUDDY_SECRET_MY_BOT_V2_OCTOTOKEN"
	if got != want {
		t.Fatalf("envName = %q, want %q", got, want)
	}
}

// TestFileBackendRoundTrip exercises the headless fallback Get/Set/Delete
// path the desktop uses when the OS keychain is unavailable (CI, ssh
// session, broken libsecret). HOME is redirected via t.Setenv +
// USERPROFILE (for Windows os.UserHomeDir) so we don't write into the
// real ~/.octobuddy.
func TestFileBackendRoundTrip(t *testing.T) {
	withHome(t, t.TempDir())
	const bot = "test-bot"
	be := fileBackend{}

	if v, err := be.Get(bot, wire.SecretKindOcto); err == nil || v != "" {
		t.Fatalf("fresh fetch must miss: v=%q err=%v", v, err)
	}

	if err := be.Set(bot, wire.SecretKindOcto, "bf_secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, err := be.Get(bot, wire.SecretKindOcto); err != nil || v != "bf_secret" {
		t.Fatalf("Get after Set: v=%q err=%v", v, err)
	}

	// Delete is the path that overwrites with empty / removes the file —
	// must NOT leave a readable stale token.
	if err := be.Delete(bot, wire.SecretKindOcto); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if v, err := be.Get(bot, wire.SecretKindOcto); err == nil || v != "" {
		t.Fatalf("Get after Delete must miss: v=%q err=%v", v, err)
	}
}

// TestFileBackendPermissionsRefuseTraversal: the file backend derives
// its path via the same account() guard, so an attacker-supplied id
// can't escape the per-user secrets dir even on the fallback path.
func TestFileBackendPermissionsRefuseTraversal(t *testing.T) {
	withHome(t, t.TempDir())
	if err := (fileBackend{}).Set("../other", wire.SecretKindOcto, "x"); err == nil {
		t.Fatal("Set('../other', …) must refuse the traversal id")
	}
}

// TestFileBackendCreates0600File asserts the on-disk file is mode 0600 —
// the file backend's whole point is keeping the secret off the
// world-readable filesystem. POSIX-only: Windows surfaces 0666/0777 from
// os.Stat regardless of the open mode (it only tracks the read-only bit),
// so the perm-bit assertion would always fail there.
func TestFileBackendCreates0600File(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows doesn't honor POSIX file modes; ACLs are the real guard there")
	}
	home := t.TempDir()
	withHome(t, home)
	const bot = "perm-test"
	if err := (fileBackend{}).Set(bot, wire.SecretKindGateway, "x"); err != nil {
		t.Fatal(err)
	}
	path, err := secretFile(bot, wire.SecretKindGateway)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("secret file perms = %04o, want 0600", perm)
	}
	// Parent dir should be 0700 (also private).
	pst, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := pst.Mode().Perm(); perm != 0o700 {
		t.Fatalf("secrets dir perms = %04o, want 0700", perm)
	}
}

// TestEnvBackendRespectsLookupEnv exercises the read-only CI fallback.
func TestEnvBackendRespectsLookupEnv(t *testing.T) {
	const bot = "ci-bot"
	envName := secretEnvName(bot, wire.SecretKindGateway)
	t.Setenv(envName, "from-env")

	if v, err := (envBackend{}).Get(bot, wire.SecretKindGateway); err != nil || v != "from-env" {
		t.Fatalf("envBackend.Get: v=%q err=%v", v, err)
	}

	// Read-only: Set + Delete must error.
	if err := (envBackend{}).Set(bot, wire.SecretKindGateway, "x"); err == nil {
		t.Fatal("envBackend.Set must refuse")
	}
	if err := (envBackend{}).Delete(bot, wire.SecretKindGateway); err == nil {
		t.Fatal("envBackend.Delete must refuse")
	}
}

// withHome is the package-local shim around desktest.WithHome — keeps
// call sites concise. See desktest.WithHome for the cross-platform
// rationale (HOME + USERPROFILE).
func withHome(t *testing.T, dir string) {
	t.Helper()
	desktest.WithHome(t, dir)
}
