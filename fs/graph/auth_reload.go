package graph

import (
	"encoding/json"
	"os"

	"github.com/rs/zerolog/log"
)

// ReloadFromFile picks up a new login written to this Auth's token file by
// another process (for example "onedriver --auth-only <mountpoint>" run while
// the mount's service keeps running). If the file holds a different refresh
// token than the one in memory, the tokens are swapped in and the expired
// state is cleared. Returns true if new tokens were loaded.
//
// This is how a mount whose login expired gets going again without a tray or
// D-Bus interface: the running instance never opens a login window itself.
func (a *Auth) ReloadFromFile() bool {
	a.mu.RLock()
	path, current := a.path, a.RefreshToken
	a.mu.RUnlock()
	if path == "" {
		return false
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var onDisk Auth
	if err := json.Unmarshal(contents, &onDisk); err != nil {
		return false
	}
	if onDisk.RefreshToken == "" || onDisk.AccessToken == "" ||
		onDisk.RefreshToken == current {
		return false
	}
	a.replaceTokens(&onDisk)
	log.Info().Msg("Picked up a new login from the token file.")
	return true
}
