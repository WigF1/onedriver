package graph

// SetExpiresAt sets the time (unix seconds) the access token expires, which
// makes the next request renew it if it is in the past. Meant for tests.
func (a *Auth) SetExpiresAt(unix int64) {
	a.mu.Lock()
	a.ExpiresAt = unix
	a.mu.Unlock()
}

// ExpiryTime returns the time (unix seconds) the access token expires.
func (a *Auth) ExpiryTime() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExpiresAt
}
