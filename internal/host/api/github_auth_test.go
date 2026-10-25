package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// githubRoundTripper 为 OAuth 流程提供不出网的 GitHub 响应。
type githubRoundTripper struct{}

// RoundTrip 返回令牌交换和用户查询的固定响应。
func (githubRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/login/oauth/access_token" {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"server-only"}`)), Header: make(http.Header), Request: r}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"login":"octocat","avatar_url":"https://github.com/octocat.png"}`)), Header: make(http.Header), Request: r}, nil
}

// TestGitHubOAuthStateAndSession 验证 state/PKCE、CSRF 拒绝以及令牌不返回浏览器。
func TestGitHubOAuthStateAndSession(t *testing.T) {
	app, err := New("http://127.0.0.1:1234", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureGitHub(GitHubConfig{ClientID: "client", ClientSecret: "secret", RedirectURL: "http://127.0.0.1:1234/api/auth/callback", HTTPClient: &http.Client{Transport: githubRoundTripper{}}}); err != nil {
		t.Fatal(err)
	}
	h := app.Handler([]byte("page"), nil)
	login := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234/api/auth/login", nil)
	login.Header.Set("Authorization", "Bearer "+app.token)
	login.Header.Set("Origin", app.origin)
	loginResponse := httptest.NewRecorder()
	h.ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusFound {
		t.Fatalf("login status=%d", loginResponse.Code)
	}
	state := loginResponse.Result().Cookies()[0]
	if state.Name != "safeagent_github_state" || state.Value == "" {
		t.Fatal("缺少 state cookie")
	}
	bad := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234/api/auth/callback?state=bad&code=x", nil)
	bad.AddCookie(state)
	badResponse := httptest.NewRecorder()
	h.ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("bad state status=%d", badResponse.Code)
	}
	callback := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234/api/auth/callback?state="+state.Value+"&code=x", nil)
	callback.AddCookie(state)
	callbackResponse := httptest.NewRecorder()
	h.ServeHTTP(callbackResponse, callback)
	if callbackResponse.Code != http.StatusFound {
		t.Fatalf("callback status=%d", callbackResponse.Code)
	}
	if callbackResponse.Header().Get("Location") != "/#"+app.token {
		t.Fatalf("回调未恢复本地页面令牌: %s", callbackResponse.Header().Get("Location"))
	}
	var session *http.Cookie
	for _, cookie := range callbackResponse.Result().Cookies() {
		if cookie.Name == "safeagent_github_session" {
			session = cookie
		}
	}
	if session == nil || session.Value == "" {
		t.Fatal("缺少登录会话")
	}
	status := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234/api/auth/status", nil)
	status.Header.Set("Authorization", "Bearer "+app.token)
	status.AddCookie(session)
	statusResponse := httptest.NewRecorder()
	h.ServeHTTP(statusResponse, status)
	if statusResponse.Code != http.StatusOK || strings.Contains(statusResponse.Body.String(), "server-only") || !strings.Contains(statusResponse.Body.String(), "octocat") {
		t.Fatalf("status body=%s", statusResponse.Body.String())
	}
}
