package e2e

import (
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
	"github.com/rs/zerolog"
)

// interactiveLogins counts attempts to open a login window (must stay 0).
var interactiveLogins atomic.Int32

func TestMain(m *testing.M) {
	if os.Getenv("ONEDRIVER_TEST_LOG") == "" {
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	}
	// tests must never open a login window or prompt in the terminal
	graph.SetInteractiveLogin(func(graph.AuthConfig, string) (string, error) {
		interactiveLogins.Add(1)
		return "", errors.New("interactive login attempted in a test")
	}, func() error { return nil })
	graph.StdinIsTerminal = func() bool { return false }

	// short network timeouts so outage tests run quickly (still longer than
	// the fake's upload delays)
	graph.ConnectTimeout = 2 * time.Second
	graph.ResponseTimeout = 4 * time.Second
	graph.NetworkDownFor = 2 * time.Second

	stop := startRouter()
	code := m.Run()
	stop()
	os.Exit(code)
}
