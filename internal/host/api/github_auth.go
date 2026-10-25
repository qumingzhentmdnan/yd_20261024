package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GitHubConfig 是 GitHub OAuth 应用的公开配置；客户端密钥只驻留 Host 内存。
type GitHubConfig struct {
	// ClientID 是 GitHub OAuth App 的客户端标识。
	ClientID string
	// ClientSecret 是 GitHub OAuth App 的客户端密钥。
	ClientSecret string
	// RedirectURL 是已在 GitHub 注册的精确回调地址。
	RedirectURL string
	// HTTPClient 用于测试或受控网络请求；为空使用默认客户端。
	HTTPClient *http.Client
}

// githubAuth 保存短时 OAuth 事务与不可持久化的登录会话。
type githubAuth struct {
	// config 是 OAuth 应用配置。
	config GitHubConfig
	// client 是 GitHub API HTTP 客户端。
	client *http.Client
	// mu 保护 pending 与 sessions。
	mu sync.Mutex
	// pending 保存一次性 state、PKCE verifier 及过期时间。
	pending map[string]githubPending
	// sessions 保存随机会话标识到 GitHub 用户摘要的映射。
	sessions map[string]githubUser
}

// githubPending 是待完成 OAuth 授权的短时状态。
type githubPending struct {
	// verifier 是 PKCE code verifier。
	verifier string
	// expires 是 state 失效时间。
	expires time.Time
	// localToken 是登录前页面的本地会话令牌，仅用于回调后恢复页面上下文。
	localToken string
}

// githubUser 是可以安全返回浏览器的 GitHub 用户摘要。
type githubUser struct {
	// Login 是 GitHub 登录名。
	Login string `json:"login"`
	// AvatarURL 是 GitHub 头像地址。
	AvatarURL string `json:"avatar_url,omitempty"`
}

// ConfigureGitHub 启用 GitHub OAuth；配置错误会被立即拒绝。
func (s *Server) ConfigureGitHub(config GitHubConfig) error {
	if config.ClientID == "" || config.ClientSecret == "" {
		return errors.New("GitHub OAuth 配置不完整")
	}
	u, err := url.Parse(config.RedirectURL)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("GitHub OAuth 回调地址无效")
	}
	client := config.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	s.github = &githubAuth{config: config, client: client, pending: make(map[string]githubPending), sessions: make(map[string]githubUser)}
	return nil
}

// githubRandom 生成 OAuth state、PKCE verifier 或会话标识。
func githubRandom(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// githubAuthHandler 处理 OAuth 登录、回调、状态和登出，不返回供应商令牌。
func (s *Server) githubAuthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	auth := s.github
	if auth == nil {
		if r.URL.Path == "/api/auth/status" {
			json.NewEncoder(w).Encode(map[string]any{"enabled": false, "authenticated": false})
			return
		}
		http.Error(w, "GitHub 登录未配置", http.StatusServiceUnavailable)
		return
	}
	switch r.URL.Path {
	case "/api/auth/status":
		user, ok := auth.session(r)
		result := map[string]any{"enabled": true, "authenticated": ok}
		if ok {
			result["user"] = user
		}
		json.NewEncoder(w).Encode(result)
	case "/api/auth/login":
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "仅支持GET或POST", 405)
			return
		}
		state, err := githubRandom(32)
		if err != nil {
			http.Error(w, "无法创建登录状态", 500)
			return
		}
		verifier, err := githubRandom(48)
		if err != nil {
			http.Error(w, "无法创建登录状态", 500)
			return
		}
		auth.mu.Lock()
		for key, value := range auth.pending {
			if time.Now().After(value.expires) {
				delete(auth.pending, key)
			}
		}
		localToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		auth.pending[state] = githubPending{verifier: verifier, expires: time.Now().Add(10 * time.Minute), localToken: localToken}
		auth.mu.Unlock()
		cookie := &http.Cookie{Name: "safeagent_github_state", Value: state, Path: "/api/auth", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(auth.config.RedirectURL, "https://"), MaxAge: 600}
		http.SetCookie(w, cookie)
		hash := sha256.Sum256([]byte(verifier))
		params := url.Values{"client_id": {auth.config.ClientID}, "redirect_uri": {auth.config.RedirectURL}, "response_type": {"code"}, "scope": {"read:user"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}}
		loginURL := "https://github.com/login/oauth/authorize?" + params.Encode()
		if r.Method == http.MethodPost {
			json.NewEncoder(w).Encode(map[string]string{"url": loginURL})
			return
		}
		http.Redirect(w, r, loginURL, http.StatusFound)
	case "/api/auth/callback":
		if r.Method != http.MethodGet {
			http.Error(w, "仅支持GET", 405)
			return
		}
		s.verifyCallback(w, r, auth)
	case "/api/auth/logout":
		if r.Method != http.MethodPost {
			http.Error(w, "仅支持POST", 405)
			return
		}
		if c, err := r.Cookie("safeagent_github_session"); err == nil {
			auth.mu.Lock()
			delete(auth.sessions, c.Value)
			auth.mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: "safeagent_github_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

// verifyCallback 校验一次性 state 与 PKCE 后换取用户摘要。
func (s *Server) verifyCallback(w http.ResponseWriter, r *http.Request, auth *githubAuth) {
	stateCookie, err := r.Cookie("safeagent_github_state")
	if err != nil || r.URL.Query().Get("state") == "" || subtle.ConstantTimeCompare([]byte(stateCookie.Value), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "登录状态无效", 400)
		return
	}
	auth.mu.Lock()
	pending, ok := auth.pending[stateCookie.Value]
	delete(auth.pending, stateCookie.Value)
	auth.mu.Unlock()
	if !ok || time.Now().After(pending.expires) || r.URL.Query().Get("code") == "" {
		http.Error(w, "登录状态已过期", 400)
		return
	}
	form := url.Values{"client_id": {auth.config.ClientID}, "client_secret": {auth.config.ClientSecret}, "code": {r.URL.Query().Get("code")}, "redirect_uri": {auth.config.RedirectURL}, "code_verifier": {pending.verifier}}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		http.Error(w, "GitHub 登录失败", 502)
		return
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := auth.client.Do(request)
	if err != nil {
		http.Error(w, "GitHub 登录失败", 502)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		http.Error(w, "GitHub 登录失败", 502)
		return
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(response.Body).Decode(&token) != nil || token.AccessToken == "" {
		http.Error(w, "GitHub 登录失败", 502)
		return
	}
	request, err = http.NewRequestWithContext(r.Context(), http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		http.Error(w, "GitHub 登录失败", 502)
		return
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "safeagent")
	response, err = auth.client.Do(request)
	if err != nil {
		http.Error(w, "GitHub 登录失败", 502)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		http.Error(w, "GitHub 登录失败", 502)
		return
	}
	var user githubUser
	if json.NewDecoder(response.Body).Decode(&user) != nil || user.Login == "" {
		http.Error(w, "GitHub 登录失败", 502)
		return
	}
	session, err := githubRandom(32)
	if err != nil {
		http.Error(w, "无法创建登录会话", 500)
		return
	}
	auth.mu.Lock()
	auth.sessions[session] = user
	auth.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "safeagent_github_session", Value: session, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(auth.config.RedirectURL, "https://"), MaxAge: 86400})
	location := "/"
	if pending.localToken != "" {
		location += "#" + url.PathEscape(pending.localToken)
	}
	http.Redirect(w, r, location, http.StatusFound)
}

// session 返回当前浏览器的 GitHub 用户摘要。
func (a *githubAuth) session(r *http.Request) (githubUser, bool) {
	c, err := r.Cookie("safeagent_github_session")
	if err != nil {
		return githubUser{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.sessions[c.Value]
	return u, ok
}
