package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
)

// Concurrent requests with an expired access token renew it once, instead
// of racing each other with the same single-use refresh token (#493).
func TestConcurrentRequestsRenewTokenOnce(t *testing.T) {
	g := newFakeGraph()
	for i := 0; i < 16; i++ {
		g.add(fmt.Sprintf("f%02d.txt", i), []byte("content"))
	}
	m := mount(t, g, options{})
	g.setTokenMode("", 300*time.Millisecond)
	m.auth.SetExpiresAt(time.Now().Add(-time.Minute).Unix())

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := os.ReadFile(filepath.Join(m.dir, fmt.Sprintf("f%02d.txt", i))); err != nil {
				t.Errorf("read %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if n := g.posts(); n != 1 {
		t.Fatalf("got %d token renewals, want 1", n)
	}
	if m.auth.Token() == m.token {
		t.Fatal("access token was not renewed")
	}
}

// Renewed (rotated) refresh tokens are saved to the token file.
func TestRenewedTokensAreSaved(t *testing.T) {
	g := newFakeGraph()
	g.add("a.txt", []byte("a"))
	m := mount(t, g, options{})
	m.auth.SetExpiresAt(time.Now().Add(-time.Minute).Unix())
	os.ReadFile(filepath.Join(m.dir, "a.txt"))

	var saved graph.Auth
	data, _ := os.ReadFile(m.tokenFile)
	json.Unmarshal(data, &saved)
	if saved.AccessToken != m.auth.Token() || saved.RefreshToken != "refresh-for-"+m.auth.Token() {
		t.Fatalf("token file not updated after renewal: %+v", saved.AccessToken)
	}
}

// A failing token endpoint (network trouble, 5xx) keeps the old token in use
// and backs off, instead of forcing a new login (#493, #478).
func TestTransientRenewalFailureKeepsWorking(t *testing.T) {
	g := newFakeGraph()
	for i := 0; i < 5; i++ {
		g.add(fmt.Sprintf("f%d.txt", i), []byte("content"))
	}
	m := mount(t, g, options{})
	g.setTokenMode("500", 0)
	m.auth.SetExpiresAt(time.Now().Add(-time.Minute).Unix())

	for i := 0; i < 5; i++ {
		if _, err := os.ReadFile(filepath.Join(m.dir, fmt.Sprintf("f%d.txt", i))); err != nil {
			t.Fatalf("read %d failed during token endpoint outage: %v", i, err)
		}
	}
	if n := g.posts(); n != 1 {
		t.Fatalf("got %d renewal attempts, want 1 (then back off)", n)
	}
	if m.auth.Expired() {
		t.Fatal("a server error expired the login")
	}
	if n := interactiveLogins.Load(); n != 0 {
		t.Fatalf("%d interactive logins attempted", n)
	}
}

// Graph rejecting the access token (401) renews it and retries the request
// without any login window.
func TestUnauthorizedRenewsAndRetries(t *testing.T) {
	g := newFakeGraph()
	g.add("a.txt", []byte("hello"))
	m := mount(t, g, options{})
	g.reject(m.token)

	got, err := os.ReadFile(filepath.Join(m.dir, "a.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("got %q, %v", got, err)
	}
	if n := g.posts(); n != 1 {
		t.Fatalf("got %d renewals, want 1", n)
	}
	if n := interactiveLogins.Load(); n != 0 {
		t.Fatalf("%d interactive logins attempted", n)
	}
}

// A rejected refresh token (invalid_grant) expires the login: no login
// window is opened from the background, onedriver keeps running and serves
// what's cached, and a login written to the token file by another process
// is picked up (#478, #467, #480).
func TestExpiredLoginKeepsMountAliveAndRecovers(t *testing.T) {
	g := newFakeGraph()
	g.add("cached.txt", []byte("cached"))
	g.add("uncached.txt", []byte("not yet downloaded"))
	m := mount(t, g, options{deltaInterval: 100 * time.Millisecond})
	if _, err := os.ReadFile(filepath.Join(m.dir, "cached.txt")); err != nil {
		t.Fatal(err)
	}

	g.setTokenMode("invalid_grant", 0)
	g.reject(m.token)
	m.auth.SetExpiresAt(time.Now().Add(-time.Minute).Unix())
	eventually(t, 5*time.Second, "login to be marked expired", m.auth.Expired)

	// still mounted and serving the cache
	if got, err := os.ReadFile(filepath.Join(m.dir, "cached.txt")); err != nil || string(got) != "cached" {
		t.Fatalf("cached file unreadable after login expired: %q, %v", got, err)
	}
	if _, err := os.ReadDir(m.dir); err != nil {
		t.Fatalf("listing failed after login expired: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(m.dir, "uncached.txt")); err == nil {
		t.Fatal("uncached file readable without a login")
	}
	if n := interactiveLogins.Load(); n != 0 {
		t.Fatalf("%d interactive logins attempted from the background", n)
	}

	// "onedriver --auth-only" in another process writes a fresh login
	fresh := router.register(g)
	data, _ := os.ReadFile(m.tokenFile)
	var onDisk map[string]interface{}
	json.Unmarshal(data, &onDisk)
	onDisk["access_token"] = fresh
	onDisk["refresh_token"] = "refresh-for-" + fresh
	onDisk["expires_at"] = time.Now().Add(time.Hour).Unix()
	data, _ = json.Marshal(onDisk)
	os.WriteFile(m.tokenFile, data, 0600)

	if !m.auth.ReloadFromFile() {
		t.Fatal("new login in the token file was not picked up")
	}
	if m.auth.Expired() || m.auth.Token() != fresh {
		t.Fatal("auth still expired after reload")
	}
	got, err := os.ReadFile(filepath.Join(m.dir, "uncached.txt"))
	if err != nil || string(got) != "not yet downloaded" {
		t.Fatalf("read after re-login: %q, %v", got, err)
	}
	if m.auth.ReloadFromFile() {
		t.Fatal("reloading an unchanged token file reported new tokens")
	}
}
