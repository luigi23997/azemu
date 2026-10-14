// Package graph holds the signed-in user's delegated Microsoft Graph token: device-code login and refresh.
// azemu's proxy swaps it in for the fake app-registration token apps get from the emulated Entra token endpoint.
package graph

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/luigi23997/azemu/internal/host"
)

const (
	Authority = "https://login.microsoftonline.com"
	TokenFile = "token.json"

	DefaultTenant = "organizations"
	// DefaultClientID is the public "Microsoft Graph Command Line Tools" app: any user can consent to delegated scopes for it
	// unless the tenant blocks it, in which case set graph.client_id to your own public client.
	DefaultClientID = "14d82eec-204b-4c2f-b7e8-296a70dab67e"
)

// DefaultScopes is read-only access to SharePoint and OneDrive.
var DefaultScopes = []string{"Sites.Read.All", "Files.Read.All"}

// Token is what is kept on disk (mode 0600): the refresh token plus the last access token.
type Token struct {
	Tenant   string    `json:"tenant"`
	ClientID string    `json:"client_id"`
	Scope    string    `json:"scope"`
	Account  string    `json:"account,omitempty"`
	Refresh  string    `json:"refresh_token"`
	Access   string    `json:"access_token"`
	Expires  time.Time `json:"expires"`
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

// Load reads the token file.
func Load(path string) (*Token, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &t, nil
}

// Save writes t atomically (temp file in the same directory, then rename).
func Save(path string, t *Token) error {
	b, _ := json.MarshalIndent(t, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil { // CreateTemp is already 0600
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func post(ctx context.Context, c *http.Client, endpoint string, form url.Values, out *tokenResp) error {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: HTTP %d: %.200s", endpoint, resp.StatusCode, b)
	}
	return nil
}

// Source returns a valid access token from the file at Path, refreshing it when it is about to expire.
type Source struct {
	Path      string
	Client    *http.Client // default http.DefaultClient
	Authority string       // default Authority
	mu        sync.Mutex
}

func (s *Source) authority() string {
	if s.Authority != "" {
		return s.Authority
	}
	return Authority
}

func (s *Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: host.DialContext, ForceAttemptHTTP2: true}}
}

func (s *Source) Access(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := Load(s.Path)
	if err != nil {
		return "", errors.New("no Graph login: run `azemu graph login` on the host")
	}
	if time.Until(t.Expires) > 2*time.Minute && t.Access != "" {
		return t.Access, nil
	}
	var r tokenResp
	err = post(ctx, s.client(), s.authority()+"/"+t.Tenant+"/oauth2/v2.0/token", url.Values{
		"client_id": {t.ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {t.Refresh}, "scope": {t.Scope},
	}, &r)
	if err != nil {
		return "", err
	}
	if r.Error != "" {
		return "", fmt.Errorf("Graph token refresh failed (%s): %s; run `azemu graph login` again", r.Error, firstLine(r.Description))
	}
	t.Access, t.Expires = r.AccessToken, time.Now().Add(time.Duration(r.ExpiresIn)*time.Second)
	if r.RefreshToken != "" {
		t.Refresh = r.RefreshToken
	}
	if err := Save(s.Path, t); err != nil {
		return "", err
	}
	return t.Access, nil
}

func firstLine(s string) string { return strings.SplitN(s, "\r\n", 2)[0] }

// sleep is swapped in tests.
var sleep = func(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Login runs the device-code flow, printing the sign-in instructions to out, and returns the new token.
func Login(ctx context.Context, c *http.Client, authority, tenant, clientID string, scopes []string, out io.Writer) (*Token, error) {
	scope := strings.Join(append(append([]string{}, scopes...), "offline_access", "openid", "profile"), " ")
	base := authority + "/" + tenant + "/oauth2/v2.0/"
	var dc struct {
		DeviceCode string `json:"device_code"`
		Interval   int    `json:"interval"`
		ExpiresIn  int    `json:"expires_in"`
		Message    string `json:"message"`
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"devicecode", strings.NewReader(url.Values{"client_id": {clientID}, "scope": {scope}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err := json.Unmarshal(b, &dc); err != nil || dc.DeviceCode == "" {
		return nil, fmt.Errorf("device code request failed: HTTP %d: %.300s", resp.StatusCode, b)
	}
	fmt.Fprintln(out, dc.Message)

	wait := time.Duration(dc.Interval) * time.Second
	if wait <= 0 {
		wait = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		sleep(ctx, wait)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var r tokenResp
		err := post(ctx, c, base+"token", url.Values{
			"client_id": {clientID}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {dc.DeviceCode},
		}, &r)
		if err != nil {
			return nil, err
		}
		switch r.Error {
		case "":
			return &Token{
				Tenant: tenant, ClientID: clientID, Scope: scope, Account: upn(r.IDToken),
				Refresh: r.RefreshToken, Access: r.AccessToken, Expires: time.Now().Add(time.Duration(r.ExpiresIn) * time.Second),
			}, nil
		case "authorization_pending":
		case "slow_down":
			wait += 5 * time.Second
		default:
			return nil, fmt.Errorf("login failed (%s): %s", r.Error, firstLine(r.Description))
		}
	}
	return nil, errors.New("login timed out")
}

// upn extracts preferred_username from an id_token (display only; never verified).
func upn(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Name string `json:"preferred_username"`
	}
	json.Unmarshal(b, &c)
	return c.Name
}
