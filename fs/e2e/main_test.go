package e2e

import (
	"errors"
	"os"
	"sync/atomic"
	"testing"

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

	stop := startRouter()
	code := m.Run()
	stop()
	os.Exit(code)
}
