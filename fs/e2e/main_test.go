package e2e

import (
	"os"
	"testing"

	"github.com/rs/zerolog"
)

func TestMain(m *testing.M) {
	if os.Getenv("ONEDRIVER_TEST_LOG") == "" {
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	}
	stop := startRouter()
	code := m.Run()
	stop()
	os.Exit(code)
}
