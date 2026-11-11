package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dario.cat/mergo"
	"github.com/rs/zerolog/log"
)

// these are default values if not specified
const (
	authClientID    = "3470c3fa-bc10-45ab-a0a9-2d30836485d1"
	authCodeURL     = "https://login.microsoftonline.com/common/oauth2/v2.0/authorize"
	authTokenURL    = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	authRedirectURL = "https://login.live.com/oauth20_desktop.srf"
)

func (a *AuthConfig) applyDefaults() error {
	return mergo.Merge(a, AuthConfig{
		ClientID:    authClientID,
		CodeURL:     authCodeURL,
		TokenURL:    authTokenURL,
		RedirectURL: authRedirectURL,
	})
}

// AuthConfig configures the authentication flow
type AuthConfig struct {
	ClientID    string `json:"clientID" yaml:"clientID"`
	CodeURL     string `json:"codeURL" yaml:"codeURL"`
	TokenURL    string `json:"tokenURL" yaml:"tokenURL"`
	RedirectURL string `json:"redirectURL" yaml:"redirectURL"`
}

// Auth represents a set of oauth2 authentication tokens. Code that may run
// concurrently with a renewal or login must use Token() to read the access
// token.
type Auth struct {
	AuthConfig   `json:"config"`
	Account      string `json:"account"`
	ExpiresIn    int64  `json:"expires_in"` // only used for parsing
	ExpiresAt    int64  `json:"expires_at"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	path         string // auth tokens remember their path for use by Refresh()

	mu        sync.RWMutex // protects the fields above and expired
	expired   bool         // the login is gone, only Login() can fix this
	refreshMu sync.Mutex   // serializes token renewals
	loginBusy int32        // only one interactive login at a time (atomic)

	// transient renewal failure bookkeeping, protected by refreshMu
	failures   int
	nextRetry  time.Time
	warnedOnce bool
}

// AuthError is an authentication error from the Microsoft API. Generally we don't see
// these unless something goes catastrophically wrong with Microsoft's authentication
// services.
type AuthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	ErrorCodes       []int  `json:"error_codes"`
	ErrorURI         string `json:"error_uri"`
	Timestamp        string `json:"timestamp"` // json.Unmarshal doesn't like this timestamp format
	TraceID          string `json:"trace_id"`
	CorrelationID    string `json:"correlation_id"`
}

// ToFile writes auth tokens to a file
func (a *Auth) ToFile(file string) error {
	a.mu.Lock()
	a.path = file
	byteData, _ := json.Marshal(a)
	a.mu.Unlock()
	return os.WriteFile(file, byteData, 0600)
}

// FromFile populates an auth struct from a file
func (a *Auth) FromFile(file string) error {
	contents, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.path = file
	err = json.Unmarshal(contents, a)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	return a.applyDefaults()
}

// Token returns the current access token.
func (a *Auth) Token() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// Expired returns true if the login is gone (the refresh token was rejected)
// and the user has to log in again with Login().
func (a *Auth) Expired() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.expired
}

// ErrAuthExpired is returned by requests while the login is expired. It does
// not look like an HTTP error, so IsOffline() is true for it and the code
// paths that handle being offline apply.
var ErrAuthExpired = errors.New("login expired, log in again")

// AuthExpiredMessage is shown to the user while the login is expired.
const AuthExpiredMessage = "Login expired"

// ReloginHint tells the user how to log in again while onedriver keeps
// running; the running instance picks up the new tokens by itself (see
// ReloadFromFile).
const ReloginHint = "Log in again with \"onedriver --auth-only <mountpoint>\" " +
	"(add -n to use your normal web browser); the running mount picks up the " +
	"new login automatically."

// MarkExpired puts the Auth into the expired state (without touching the
// tokens on disk) and calls the renewal failure handlers once. It is a no-op
// if the Auth was expired already.
func (a *Auth) MarkExpired() {
	a.mu.Lock()
	was := a.expired
	a.expired = true
	a.mu.Unlock()
	if !was {
		a.notifyRenewalFailure(AuthExpiredMessage)
	}
}

var (
	failureM        sync.Mutex
	failureHandlers = map[*Auth]map[int]func(string){}
	failureNext     int
)

// OnRenewalFailure registers a function that is called (before the login is
// attempted again) whenever renewing the access tokens fails and the user must
// authenticate again. It must not block. Several handlers may be registered;
// the returned function removes this one.
func (a *Auth) OnRenewalFailure(handler func(msg string)) (cancel func()) {
	failureM.Lock()
	defer failureM.Unlock()
	if failureHandlers[a] == nil {
		failureHandlers[a] = map[int]func(string){}
	}
	id := failureNext
	failureNext++
	failureHandlers[a][id] = handler
	return func() {
		failureM.Lock()
		defer failureM.Unlock()
		delete(failureHandlers[a], id)
		if len(failureHandlers[a]) == 0 {
			delete(failureHandlers, a)
		}
	}
}

func (a *Auth) notifyRenewalFailure(msg string) {
	failureM.Lock()
	var handlers []func(string)
	for _, h := range failureHandlers[a] {
		handlers = append(handlers, h)
	}
	failureM.Unlock()
	for _, h := range handlers {
		h(msg)
	}
}

// Refresh renews the access token if it is expired. Failures are handled
// internally: transient ones keep the old token (and are retried later with a
// backoff), a rejected refresh token marks the login as expired, see Expired().
func (a *Auth) Refresh() {
	_ = a.refresh("")
}

const (
	maxRefreshBackoff = 5 * time.Minute
	refreshTimeout    = 30 * time.Second
)

var errRefreshBackoff = errors.New("token renewal failed recently, not retrying yet")

// refresh renews the tokens. If usedToken is empty, this only happens when the
// access token is past its expiry time. Otherwise the server rejected
// usedToken, and the tokens are renewed regardless of their expiry time unless
// someone else did that already. Returns nil if the current access token may
// be used, ErrAuthExpired if the login is gone and another error for
// transient failures.
func (a *Auth) refresh(usedToken string) error {
	if a.Expired() {
		return ErrAuthExpired
	}
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	if a.Expired() {
		return ErrAuthExpired
	}

	a.mu.RLock()
	current, expiresAt, refreshToken := a.AccessToken, a.ExpiresAt, a.RefreshToken
	cfg := a.AuthConfig
	a.mu.RUnlock()
	if usedToken == "" && expiresAt > time.Now().Unix() {
		return nil
	}
	if usedToken != "" && current != usedToken {
		return nil // already renewed by someone else
	}
	if time.Now().Before(a.nextRetry) {
		return errRefreshBackoff
	}

	tok, transient, err := postTokenRequest(cfg.TokenURL, url.Values{
		"client_id":     {cfg.ClientID},
		"redirect_uri":  {cfg.RedirectURL},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	})
	if err != nil {
		if transient {
			a.failures++
			shift := a.failures - 1
			if shift > 6 {
				shift = 6
			}
			delay := 5 * time.Second << uint(shift)
			if delay > maxRefreshBackoff {
				delay = maxRefreshBackoff
			}
			a.nextRetry = time.Now().Add(delay)
			if !a.warnedOnce {
				a.warnedOnce = true
				log.Warn().Err(err).Dur("retryIn", delay).
					Msg("Could not renew the access token, will keep trying in the background.")
			} else {
				log.Debug().Err(err).Dur("retryIn", delay).Msg("Token renewal still failing.")
			}
			return err
		}
		log.Error().Err(err).Msg("Login expired: the refresh token was rejected. " +
			"The filesystem keeps running from its cache until the user logs in again.")
		a.MarkExpired()
		return ErrAuthExpired
	}
	if a.failures > 0 || a.warnedOnce {
		log.Info().Msg("Token renewal works again.")
	}
	a.failures, a.nextRetry, a.warnedOnce = 0, time.Time{}, false

	a.mu.Lock()
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	a.ExpiresAt = tok.ExpiresAt
	path := a.path
	a.mu.Unlock()
	if path != "" {
		if err := a.ToFile(path); err != nil {
			log.Error().Err(err).Msg("Could not save the renewed tokens.")
		}
	}
	return nil
}

// postTokenRequest posts form to the token endpoint. On failure, transient
// says if retrying later may help (network problems, 5xx, 429, timeouts) as
// opposed to the server definitively refusing the request (400/401:
// invalid_grant etc).
func postTokenRequest(tokenURL string, form url.Values) (tok *Auth, transient bool, err error) {
	client := &http.Client{Timeout: refreshTimeout}
	resp, err := client.Post(tokenURL, "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var parsed Auth
	json.Unmarshal(body, &parsed)
	if resp.StatusCode == http.StatusOK && parsed.AccessToken != "" {
		parsed.ExpiresAt = time.Now().Unix() + parsed.ExpiresIn
		return &parsed, false, nil
	}

	var authErr AuthError
	json.Unmarshal(body, &authErr)
	err = fmt.Errorf("token endpoint returned status %d: %s %s",
		resp.StatusCode, authErr.Error, authErr.ErrorDescription)
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		// invalid_grant, invalid_client, ... the login is gone (unless the
		// server says it is only temporarily unavailable)
		if authErr.Error == "temporarily_unavailable" || authErr.Error == "server_error" {
			return nil, true, err
		}
		return nil, false, err
	default:
		// 5xx, 429, 200 without tokens...
		return nil, true, err
	}
}

// Get the appropriate authentication URL for the Graph OAuth2 challenge.
func getAuthURL(a AuthConfig) string {
	return a.CodeURL +
		"?client_id=" + a.ClientID +
		"&scope=" + url.PathEscape("user.read files.readwrite.all offline_access") +
		"&response_type=code" +
		"&redirect_uri=" + a.RedirectURL
}

// getAuthCodeHeadless has the user perform authentication in their own browser
// instead of WebKit2GTK and then input the auth code in the terminal.
func getAuthCodeHeadless(a AuthConfig, accountName string) (string, error) {
	fmt.Printf("Please open the following URL in your normal web browser and sign in:\n%s\n\n",
		getAuthURL(a))
	fmt.Println("After you accept \"Let this app access your info?\", your browser will " +
		"show a blank page whose address starts with\n" +
		"https://login.live.com/oauth20_desktop.srf?code=...\n" +
		"Copy that ENTIRE address from the address bar, paste it here and press Enter:")
	var response string
	fmt.Scanln(&response)
	code, err := parseAuthCode(response)
	if err != nil || code == "" {
		return "", errors.New("no validation code returned, or code was invalid")
	}
	return code, nil
}

// parseAuthCode is used to parse the auth code out of the redirect the server gives us
// after successful authentication
func parseAuthCode(authURL string) (string, error) {
	parsed, err := url.Parse(authURL)
	if err != nil {
		return "", err
	}
	params := parsed.Query()
	return params.Get("code"), nil
}

// exchangeAuthCode exchanges an auth code for a set of access tokens (returned
// as a new Auth struct).
func exchangeAuthCode(a AuthConfig, authCode string) (*Auth, error) {
	auth, _, err := postTokenRequest(a.TokenURL, url.Values{
		"client_id":    {a.ClientID},
		"redirect_uri": {a.RedirectURL},
		"code":         {authCode},
		"grant_type":   {"authorization_code"},
	})
	if err != nil {
		return nil, err
	}
	if auth.RefreshToken == "" {
		return nil, errors.New("token endpoint returned no refresh token")
	}
	auth.AuthConfig = a
	return auth, nil
}

// getAuthTokens is exchangeAuthCode for the startup path, where there is no
// way to continue without tokens.
func getAuthTokens(a AuthConfig, authCode string) *Auth {
	auth, err := exchangeAuthCode(a, authCode)
	if err != nil {
		log.Fatal().Err(err).
			Msg("Failed to retrieve access tokens. Authentication cannot continue.")
	}
	return auth
}

// newAuth performs initial authentication flow and saves tokens to disk. The headless
// parameter determines if we will try to auth directly in the terminal instead of
// doing it via embedded browser.
func newAuth(config AuthConfig, path string, headless bool) *Auth {
	// load the old account name
	old := Auth{}
	old.FromFile(path)

	config.applyDefaults()
	var code string
	var err error
	if headless {
		code, err = getAuthCodeHeadless(config, old.Account)
	} else {
		// in a build without CGO, this will be the same as above
		code, err = getAuthCode(config, old.Account)
	}
	if err != nil {
		// only reachable at startup, before anything is mounted
		log.Fatal().Err(err).Msg("Login did not complete. " +
			"Please restart the application and try again.")
	}
	auth := getAuthTokens(config, code)

	if user, err := GetUser(auth); err == nil {
		auth.Account = user.UserPrincipalName
	}
	auth.ToFile(path)
	return auth
}

// StdinIsTerminal tells if onedriver was started interactively. Tests may
// override it.
var StdinIsTerminal = func() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// Authenticate performs authentication to Graph or load auth/refreshes it
// from an existing file. If headless is true, we will authenticate in the
// terminal.
//
// Without any tokens (first run) the login always happens interactively. If
// there are tokens but the renewal is rejected (the login expired), the user
// is asked to log in again only if onedriver was started from a terminal.
// Started as a service (stdin is not a terminal), no window is opened: the
// returned Auth is in the expired state (see Expired) and the filesystem
// starts from its cache until Login() succeeds.
func Authenticate(config AuthConfig, path string, headless bool) *Auth {
	auth := &Auth{}
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		// no tokens found, gotta start oauth flow from beginning
		auth = newAuth(config, path, headless)
	} else {
		// we already have tokens, no need to force a new auth flow
		auth.FromFile(path)
		if auth.refreshTokenValue() == "" {
			auth.expired = true // unusable token file
		} else {
			auth.Refresh()
		}
		if auth.Expired() {
			if StdinIsTerminal() {
				log.Warn().Msg("Login expired, logging in again.")
				fresh := newAuth(auth.AuthConfig, path, headless)
				auth.replaceTokens(fresh)
			} else {
				log.Warn().Msg("Login expired. Not opening a login window from a " +
					"background service: starting without a login. " + ReloginHint)
			}
		}
	}
	return auth
}

func (a *Auth) refreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

// replaceTokens swaps in the tokens of fresh and clears the expired state.
func (a *Auth) replaceTokens(fresh *Auth) {
	a.mu.Lock()
	a.AccessToken = fresh.AccessToken
	a.RefreshToken = fresh.RefreshToken
	a.ExpiresAt = fresh.ExpiresAt
	if fresh.Account != "" {
		a.Account = fresh.Account
	}
	a.expired = false
	a.mu.Unlock()
	a.refreshMu.Lock()
	a.failures, a.nextRetry, a.warnedOnce = 0, time.Time{}, false
	a.refreshMu.Unlock()
}

// ErrLoginInProgress is returned by Login if another login is running.
var ErrLoginInProgress = errors.New("a login is already in progress")

// Login runs the interactive login (see SetInteractiveLogin) and, on success,
// replaces the tokens in place, saves them and clears the expired state. On
// failure the Auth is left as it was. It blocks until the user is done, only
// one Login runs at a time.
func (a *Auth) Login() error {
	if !atomic.CompareAndSwapInt32(&a.loginBusy, 0, 1) {
		return ErrLoginInProgress
	}
	defer atomic.StoreInt32(&a.loginBusy, 0)

	a.mu.RLock()
	cfg, account, path := a.AuthConfig, a.Account, a.path
	a.mu.RUnlock()
	cfg.applyDefaults()

	code, err := interactiveLogin(cfg, account)
	if err != nil {
		return err
	}
	fresh, err := exchangeAuthCode(cfg, code)
	if err != nil {
		return err
	}
	if user, err := GetUser(fresh); err == nil {
		if account != "" && user.UserPrincipalName != "" &&
			!strings.EqualFold(account, user.UserPrincipalName) {
			return fmt.Errorf("logged in as %s, but this mount belongs to %s",
				user.UserPrincipalName, account)
		}
		fresh.Account = user.UserPrincipalName
	}
	a.replaceTokens(fresh)
	if path != "" {
		if err := a.ToFile(path); err != nil {
			log.Error().Err(err).Msg("Could not save the new tokens.")
		}
	}
	log.Info().Msg("Logged in again.")
	return nil
}

var (
	loginHookM      sync.Mutex
	interactiveHook func(cfg AuthConfig, account string) (string, error)
	unavailableHook func() error
)

// SetInteractiveLogin replaces the interactive part of Login (the login
// window) and the check whether it is possible at all. It exists for tests.
// The returned function restores the defaults.
func SetInteractiveLogin(login func(cfg AuthConfig, account string) (string, error), unavailable func() error) (restore func()) {
	loginHookM.Lock()
	defer loginHookM.Unlock()
	oldLogin, oldUnavail := interactiveHook, unavailableHook
	interactiveHook, unavailableHook = login, unavailable
	return func() {
		loginHookM.Lock()
		defer loginHookM.Unlock()
		interactiveHook, unavailableHook = oldLogin, oldUnavail
	}
}

func interactiveLogin(cfg AuthConfig, account string) (string, error) {
	loginHookM.Lock()
	hook := interactiveHook
	loginHookM.Unlock()
	if hook != nil {
		return hook(cfg, account)
	}
	if err := LoginUnavailable(); err != nil {
		return "", err
	}
	return getAuthCode(cfg, account)
}

// NoLoginWindow disables the login window, set when running with -n.
var NoLoginWindow bool

// LoginUnavailable returns an error explaining why Login cannot open a login
// window, or nil if it can.
func LoginUnavailable() error {
	loginHookM.Lock()
	hook := unavailableHook
	loginHookM.Unlock()
	if hook != nil {
		return hook()
	}
	if NoLoginWindow || (os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "") {
		return errors.New("no login window available (headless mode or no display): " +
			"run \"onedriver -n <mountpoint>\" in a terminal to log in again")
	}
	return nil
}
