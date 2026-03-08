package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	xdk "github.com/missuo/xdk-go"
)

const (
	defaultServerAddr  = ":8080"
	defaultRedirectURI = "http://localhost:9100"
	maxMediaCount      = 4
	maxMediaBytes      = 8 * 1024 * 1024
)

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(home, ".config", "xpost", "config.json")
}

type Config struct {
	Server   ServerConfig   `json:"server"`
	Security SecurityConfig `json:"security"`
	X        XAuthConfig    `json:"x"`
}

type ServerConfig struct {
	Addr string `json:"addr"`
}

type SecurityConfig struct {
	APIToken string `json:"api_token"`
}

type XAuthConfig struct {
	APIKey             string   `json:"api_key,omitempty"`
	APISecret          string   `json:"api_secret,omitempty"`
	AccessToken        string   `json:"access_token,omitempty"`
	AccessTokenSecret  string   `json:"access_token_secret,omitempty"`
	UserID             string   `json:"user_id,omitempty"`
	OAuth2ClientID     string   `json:"oauth2_client_id,omitempty"`
	OAuth2ClientSecret string   `json:"oauth2_client_secret,omitempty"`
	OAuth2RedirectURI  string   `json:"oauth2_redirect_uri,omitempty"`
	OAuth2Scope        []string `json:"oauth2_scope,omitempty"`
	OAuth2AccessToken  string   `json:"oauth2_access_token,omitempty"`
	OAuth2RefreshToken string   `json:"oauth2_refresh_token,omitempty"`
	OAuth2TokenType    string   `json:"oauth2_token_type,omitempty"`
	OAuth2ExpiresAt    int64    `json:"oauth2_expires_at,omitempty"`
}

type App struct {
	mu         sync.RWMutex
	cfg        *Config
	configPath string
	persistCfg bool
	poster     *Poster
	posterErr  error
}

type Poster struct {
	client   *xdk.Client
	authMode string
}

type MediaRef struct {
	ID       string `json:"id,omitempty"`
	MediaKey string `json:"media_key,omitempty"`
}

type createTweetJSONRequest struct {
	Text              string   `json:"text"`
	MediaBase64       []string `json:"media_base64"`
	MediaContentTypes []string `json:"media_content_types"`
	ReplyToTweetID    string   `json:"reply_to_tweet_id"`
}

type mediaUploadInput struct {
	Data        []byte
	ContentType string
}

func RunLocal() error {
	configPath := os.Getenv("XPOST_CONFIG")
	if strings.TrimSpace(configPath) == "" {
		configPath = defaultConfigPath()
	}

	cfg, firstBoot, err := loadOrInitConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to initialize config: %w", err)
	}

	overrideConfigFromEnv(cfg)
	if firstBoot {
		if err := ensureFirstBootAuthConfigured(cfg.X); err != nil {
			return fmt.Errorf("first boot credential check failed: %w", err)
		}
		if err := saveConfig(configPath, cfg); err != nil {
			return fmt.Errorf("failed to persist first boot config: %w", err)
		}
	}

	app := &App{
		cfg:        cfg,
		configPath: configPath,
		persistCfg: true,
	}
	app.refreshPoster()

	if firstBoot {
		log.Printf("first boot: config initialized at %s", configPath)
		if strings.TrimSpace(os.Getenv("XPOST_API_TOKEN")) == "" {
			log.Printf("first boot: API token auto-generated, see config file")
		} else {
			log.Printf("first boot: API token loaded from XPOST_API_TOKEN")
		}
	}
	if app.posterErr != nil {
		log.Printf("x auth is not ready yet: %v", app.posterErr)
	}

	router := newRouter(app)
	log.Printf("server listening on %s", cfg.Server.Addr)
	return router.Run(cfg.Server.Addr)
}

func NewVercelHandler() (http.Handler, error) {
	cfg := &Config{
		Server: ServerConfig{
			Addr: defaultServerAddr,
		},
	}
	overrideConfigFromEnv(cfg)
	if strings.TrimSpace(cfg.Security.APIToken) == "" {
		return nil, errors.New("XPOST_API_TOKEN is required in Vercel environment")
	}
	if err := ensureFirstBootAuthConfigured(cfg.X); err != nil {
		return nil, err
	}

	app := &App{
		cfg:        cfg,
		configPath: "",
		persistCfg: false,
	}
	app.refreshPoster()
	if app.posterErr != nil {
		return nil, app.posterErr
	}
	return newRouter(app), nil
}

func newRouter(app *App) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery(), corsMiddleware())

	router.OPTIONS("/*any", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	protected := router.Group("/")
	protected.Use(app.authMiddleware())
	{
		protected.POST("/v1/tweets", app.handleCreateTweet)
		protected.GET("/v1/timeline", app.handleGetTimeline)
	}

	return router
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "POST,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization,Content-Type,X-API-Token")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func loadOrInitConfig(path string) (*Config, bool, error) {
	path = filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}

	cfg := &Config{
		Server: ServerConfig{
			Addr: defaultServerAddr,
		},
	}

	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg.Security.APIToken = generateToken()
			if err := saveConfig(path, cfg); err != nil {
				return nil, false, err
			}
			return cfg, true, nil
		}
		return nil, false, err
	}

	if len(strings.TrimSpace(string(content))) > 0 {
		if err := json.Unmarshal(content, cfg); err != nil {
			return nil, false, err
		}
	}

	changed := false
	if strings.TrimSpace(cfg.Server.Addr) == "" {
		cfg.Server.Addr = defaultServerAddr
		changed = true
	}
	if strings.TrimSpace(cfg.Security.APIToken) == "" {
		cfg.Security.APIToken = generateToken()
		changed = true
	}

	if changed {
		if err := saveConfig(path, cfg); err != nil {
			return nil, false, err
		}
	}

	return cfg, false, nil
}

func saveConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func overrideConfigFromEnv(cfg *Config) {
	if v := strings.TrimSpace(os.Getenv("XPOST_ADDR")); v != "" {
		cfg.Server.Addr = v
	}
	if v := strings.TrimSpace(os.Getenv("XPOST_API_TOKEN")); v != "" {
		cfg.Security.APIToken = v
	}

	if v := strings.TrimSpace(os.Getenv("X_API_KEY")); v != "" {
		cfg.X.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("X_API_SECRET")); v != "" {
		cfg.X.APISecret = v
	}
	if v := strings.TrimSpace(os.Getenv("X_ACCESS_TOKEN")); v != "" {
		cfg.X.AccessToken = v
	}
	if v := strings.TrimSpace(os.Getenv("X_ACCESS_TOKEN_SECRET")); v != "" {
		cfg.X.AccessTokenSecret = v
	}
	if v := strings.TrimSpace(os.Getenv("X_USER_ID")); v != "" {
		cfg.X.UserID = v
	}
	if v := strings.TrimSpace(os.Getenv("X_OAUTH2_CLIENT_ID")); v != "" {
		cfg.X.OAuth2ClientID = v
	}
	if v := strings.TrimSpace(os.Getenv("X_OAUTH2_CLIENT_SECRET")); v != "" {
		cfg.X.OAuth2ClientSecret = v
	}
	if v := strings.TrimSpace(os.Getenv("X_OAUTH2_REDIRECT_URI")); v != "" {
		cfg.X.OAuth2RedirectURI = v
	}
	if v := strings.TrimSpace(os.Getenv("X_OAUTH2_SCOPE")); v != "" {
		cfg.X.OAuth2Scope = splitCSV(v)
	}
	if v := strings.TrimSpace(os.Getenv("X_OAUTH2_ACCESS_TOKEN")); v != "" {
		cfg.X.OAuth2AccessToken = v
	}
	if v := strings.TrimSpace(os.Getenv("X_OAUTH2_REFRESH_TOKEN")); v != "" {
		cfg.X.OAuth2RefreshToken = v
	}
	if v := strings.TrimSpace(os.Getenv("X_OAUTH2_TOKEN_TYPE")); v != "" {
		cfg.X.OAuth2TokenType = v
	}
	if v := strings.TrimSpace(os.Getenv("X_OAUTH2_EXPIRES_AT")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.X.OAuth2ExpiresAt = n
		}
	}
}

func generateToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func ensureFirstBootAuthConfigured(cfg XAuthConfig) error {
	if strings.TrimSpace(cfg.OAuth2AccessToken) != "" {
		return nil
	}
	if len(missingOAuth1Fields(cfg)) == 0 {
		return nil
	}
	return errors.New("set OAuth1 credentials via X_API_KEY/X_API_SECRET/X_ACCESS_TOKEN/X_ACCESS_TOKEN_SECRET, or set X_OAUTH2_ACCESS_TOKEN")
}

func (a *App) refreshPoster() {
	a.mu.Lock()
	defer a.mu.Unlock()
	poster, err := newPoster(a.cfg.X)
	a.poster = poster
	a.posterErr = err
}

func newPoster(authCfg XAuthConfig) (*Poster, error) {
	if hasAnyOAuth1Fields(authCfg) {
		if missing := missingOAuth1Fields(authCfg); len(missing) > 0 {
			return nil, fmt.Errorf("incomplete OAuth1 config, missing: %s", strings.Join(missing, ", "))
		}

		oauth1 := xdk.NewOAuth1(
			authCfg.APIKey,
			authCfg.APISecret,
			"",
			authCfg.AccessToken,
			authCfg.AccessTokenSecret,
		)
		client := xdk.NewClient(xdk.Config{Auth: oauth1})
		return &Poster{client: client, authMode: "oauth1"}, nil
	}

	if strings.TrimSpace(authCfg.OAuth2AccessToken) != "" {
		clientCfg := xdk.Config{
			AccessToken: authCfg.OAuth2AccessToken,
		}
		if strings.TrimSpace(authCfg.OAuth2ClientID) != "" {
			clientCfg.ClientID = strings.TrimSpace(authCfg.OAuth2ClientID)
			clientCfg.ClientSecret = strings.TrimSpace(authCfg.OAuth2ClientSecret)
			clientCfg.RedirectURI = strings.TrimSpace(authCfg.OAuth2RedirectURI)
			clientCfg.Scope = effectiveOAuth2Scopes(authCfg.OAuth2Scope)
			clientCfg.Token = oauth2TokenFromConfig(authCfg)
		}
		client := xdk.NewClient(clientCfg)
		return &Poster{client: client, authMode: "oauth2_user_token"}, nil
	}

	return nil, errors.New("missing x auth configuration (set OAuth1 fields or oauth2_access_token)")
}

func hasAnyOAuth1Fields(cfg XAuthConfig) bool {
	return strings.TrimSpace(cfg.APIKey) != "" ||
		strings.TrimSpace(cfg.APISecret) != "" ||
		strings.TrimSpace(cfg.AccessToken) != "" ||
		strings.TrimSpace(cfg.AccessTokenSecret) != ""
}

func missingOAuth1Fields(cfg XAuthConfig) []string {
	var missing []string
	if strings.TrimSpace(cfg.APIKey) == "" {
		missing = append(missing, "api_key")
	}
	if strings.TrimSpace(cfg.APISecret) == "" {
		missing = append(missing, "api_secret")
	}
	if strings.TrimSpace(cfg.AccessToken) == "" {
		missing = append(missing, "access_token")
	}
	if strings.TrimSpace(cfg.AccessTokenSecret) == "" {
		missing = append(missing, "access_token_secret")
	}
	return missing
}

func (a *App) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		expected := a.getAPIToken()
		if expected == "" {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error": "api token is not configured",
			})
			return
		}

		got := readTokenFromRequest(c.Request)
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "invalid api token",
			})
			return
		}
		c.Next()
	}
}

func readTokenFromRequest(r *http.Request) string {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "Bearer ") {
		return strings.TrimSpace(authHeader[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-API-Token"))
}

func (a *App) getAPIToken() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.Security.APIToken
}

func (a *App) getPoster() (*Poster, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.poster == nil {
		if a.posterErr != nil {
			return nil, a.posterErr
		}
		return nil, errors.New("x client is not ready")
	}
	return a.poster, nil
}

func (a *App) persistConfig(cfg *Config) error {
	if !a.persistCfg || strings.TrimSpace(a.configPath) == "" {
		return nil
	}
	return saveConfig(a.configPath, cfg)
}

func (a *App) persistOAuth2Token(poster *Poster) {
	if !a.persistCfg || strings.TrimSpace(a.configPath) == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := persistOAuth2TokenIfAvailable(a.cfg, a.configPath, poster.client); err != nil {
		log.Printf("warning: failed to persist refreshed oauth2 token: %v", err)
	}
}

func (a *App) handleCreateTweet(c *gin.Context) {
	poster, err := a.getPoster()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}

	text, mediaInputs, replyToTweetID, err := parseTweetRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()

	uploaded := make([]MediaRef, 0, len(mediaInputs))
	for _, input := range mediaInputs {
		ref, err := poster.UploadMedia(ctx, input.Data, input.ContentType)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		uploaded = append(uploaded, ref)
	}

	tweetResp, err := poster.CreateTweet(ctx, text, uploaded, replyToTweetID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	a.persistOAuth2Token(poster)

	c.JSON(http.StatusOK, gin.H{
		"ok":          true,
		"auth_mode":   poster.authMode,
		"media":       uploaded,
		"tweet":       tweetResp,
		"media_count": len(uploaded),
	})
}

func (a *App) handleGetTimeline(c *gin.Context) {
	poster, err := a.getPoster()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}

	a.mu.RLock()
	userID := strings.TrimSpace(a.cfg.X.UserID)
	a.mu.RUnlock()
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X_USER_ID is not configured"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()

	params := xdk.Params{"id": userID}

	// Forward supported query parameters to X API.
	queryKeys := []string{
		"max_results", "pagination_token",
		"since_id", "until_id",
		"start_time", "end_time",
		"exclude",
		"tweet.fields", "user.fields", "media.fields",
		"expansions", "poll.fields", "place.fields",
	}
	for _, key := range queryKeys {
		if v := strings.TrimSpace(c.Query(key)); v != "" {
			paramKey := strings.ReplaceAll(key, ".", "_")
			params[paramKey] = v
		}
	}

	timeline, err := poster.GetTimeline(ctx, params)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	a.persistOAuth2Token(poster)

	c.JSON(http.StatusOK, timeline)
}

func parseTweetRequest(c *gin.Context) (string, []mediaUploadInput, string, error) {
	contentType := c.GetHeader("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		return parseMultipartTweetRequest(c)
	}
	return parseJSONTweetRequest(c)
}

func parseMultipartTweetRequest(c *gin.Context) (string, []mediaUploadInput, string, error) {
	text := strings.TrimSpace(c.PostForm("text"))
	replyToTweetID := strings.TrimSpace(c.PostForm("reply_to_tweet_id"))

	form, err := c.MultipartForm()
	if err != nil {
		return "", nil, "", fmt.Errorf("invalid multipart request: %w", err)
	}

	files := form.File["media"]
	if len(files) > maxMediaCount {
		return "", nil, "", fmt.Errorf("too many media files, max is %d", maxMediaCount)
	}
	if text == "" && len(files) == 0 {
		return "", nil, "", errors.New("text or media is required")
	}

	media := make([]mediaUploadInput, 0, len(files))
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			return "", nil, "", err
		}

		data, readErr := io.ReadAll(io.LimitReader(f, maxMediaBytes+1))
		closeErr := f.Close()
		if readErr != nil {
			return "", nil, "", readErr
		}
		if closeErr != nil {
			return "", nil, "", closeErr
		}
		if int64(len(data)) > maxMediaBytes {
			return "", nil, "", fmt.Errorf("file %q exceeds max size %d bytes", fh.Filename, maxMediaBytes)
		}

		contentType := fh.Header.Get("Content-Type")
		if strings.TrimSpace(contentType) == "" {
			contentType = http.DetectContentType(data)
		}

		media = append(media, mediaUploadInput{
			Data:        data,
			ContentType: contentType,
		})
	}

	return text, media, replyToTweetID, nil
}

func parseJSONTweetRequest(c *gin.Context) (string, []mediaUploadInput, string, error) {
	var req createTweetJSONRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return "", nil, "", err
	}

	text := strings.TrimSpace(req.Text)
	if text == "" && len(req.MediaBase64) == 0 {
		return "", nil, "", errors.New("text or media_base64 is required")
	}
	if len(req.MediaBase64) > maxMediaCount {
		return "", nil, "", fmt.Errorf("too many media items, max is %d", maxMediaCount)
	}
	if len(req.MediaContentTypes) > 0 && len(req.MediaContentTypes) != len(req.MediaBase64) {
		return "", nil, "", errors.New("media_content_types length must match media_base64 length")
	}

	media := make([]mediaUploadInput, 0, len(req.MediaBase64))
	for i, item := range req.MediaBase64 {
		raw := strings.TrimSpace(item)
		if raw == "" {
			return "", nil, "", fmt.Errorf("media_base64[%d] is empty", i)
		}
		data, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return "", nil, "", fmt.Errorf("media_base64[%d] decode failed: %w", i, err)
		}
		if int64(len(data)) > maxMediaBytes {
			return "", nil, "", fmt.Errorf("media_base64[%d] exceeds max size %d bytes", i, maxMediaBytes)
		}

		contentType := ""
		if len(req.MediaContentTypes) > 0 && strings.TrimSpace(req.MediaContentTypes[i]) != "" {
			contentType = strings.TrimSpace(req.MediaContentTypes[i])
		} else {
			contentType = http.DetectContentType(data)
		}

		media = append(media, mediaUploadInput{
			Data:        data,
			ContentType: contentType,
		})
	}

	return text, media, strings.TrimSpace(req.ReplyToTweetID), nil
}

func (p *Poster) UploadMedia(ctx context.Context, data []byte, contentType string) (MediaRef, error) {
	encoded := base64.StdEncoding.EncodeToString(data)
	mediaCategory := mediaCategoryFromType(contentType)
	attemptBodies := []map[string]any{
		{
			"media":          encoded,
			"media_type":     contentType,
			"media_category": mediaCategory,
		},
		{
			"media_data":     encoded,
			"media_type":     contentType,
			"media_category": mediaCategory,
		},
	}

	var errs []string
	for _, body := range attemptBodies {
		resp, err := p.client.Media.Upload(ctx, xdk.Params{"body": body})
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		ref := extractMediaRef(resp)
		if ref.ID != "" || ref.MediaKey != "" {
			return ref, nil
		}
		errs = append(errs, "upload returned no media identifier")
	}

	ref, err := p.uploadMediaChunked(ctx, encoded, len(data), contentType)
	if err == nil {
		return ref, nil
	}
	errs = append(errs, err.Error())

	if p.client != nil && p.client.Auth != nil {
		ref, err = p.uploadMediaV1(ctx, data, contentType)
		if err == nil {
			return ref, nil
		}
		errs = append(errs, err.Error())
	}

	return MediaRef{}, fmt.Errorf("media upload failed: %s", strings.Join(errs, " | "))
}

func (p *Poster) uploadMediaChunked(ctx context.Context, encoded string, size int, contentType string) (MediaRef, error) {
	initResp, err := p.client.Media.InitializeUpload(ctx, xdk.Params{
		"body": map[string]any{
			"total_bytes":    size,
			"media_type":     contentType,
			"media_category": mediaCategoryFromType(contentType),
		},
	})
	if err != nil {
		return MediaRef{}, err
	}

	initRef := extractMediaRef(initResp)
	mediaID := initRef.ID
	if mediaID == "" {
		return MediaRef{}, errors.New("initialize_upload did not return media id")
	}

	appendBodies := []map[string]any{
		{
			"segment_index": 0,
			"media":         encoded,
		},
		{
			"segment_index": 0,
			"media_data":    encoded,
		},
	}

	var appendErr error
	for _, body := range appendBodies {
		_, appendErr = p.client.Media.AppendUpload(ctx, xdk.Params{
			"id":   mediaID,
			"body": body,
		})
		if appendErr == nil {
			break
		}
	}
	if appendErr != nil {
		return MediaRef{}, appendErr
	}

	finalResp, err := p.client.Media.FinalizeUpload(ctx, xdk.Params{
		"id": mediaID,
	})
	if err != nil {
		return MediaRef{}, err
	}

	finalRef := extractMediaRef(finalResp)
	if finalRef.ID == "" {
		finalRef.ID = mediaID
	}
	if finalRef.MediaKey == "" {
		finalRef.MediaKey = initRef.MediaKey
	}
	return finalRef, nil
}

func (p *Poster) uploadMediaV1(ctx context.Context, data []byte, contentType string) (MediaRef, error) {
	const uploadURL = "https://upload.twitter.com/1.1/media/upload.json"
	if p.client == nil || p.client.Auth == nil {
		return MediaRef{}, errors.New("oauth1 auth is required for v1 media upload fallback")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("media", "upload")
	if err != nil {
		return MediaRef{}, err
	}
	if _, err := part.Write(data); err != nil {
		return MediaRef{}, err
	}
	if strings.TrimSpace(contentType) != "" {
		_ = writer.WriteField("media_type", contentType)
	}
	if err := writer.Close(); err != nil {
		return MediaRef{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &body)
	if err != nil {
		return MediaRef{}, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	authHeader, err := p.client.Auth.BuildRequestHeader(http.MethodPost, uploadURL, "")
	if err != nil {
		return MediaRef{}, err
	}
	req.Header.Set("Authorization", authHeader)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return MediaRef{}, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return MediaRef{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return MediaRef{}, fmt.Errorf("v1 media upload failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	var obj any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil {
		return MediaRef{}, fmt.Errorf("v1 media upload parse failed: %w", err)
	}
	ref := extractMediaRef(obj)
	if ref.ID == "" {
		return MediaRef{}, fmt.Errorf("v1 media upload returned no media id: %s", strings.TrimSpace(string(payload)))
	}
	return ref, nil
}

func mediaCategoryFromType(contentType string) string {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "tweet_image"
	case strings.HasPrefix(ct, "video/"):
		return "tweet_video"
	case strings.HasPrefix(ct, "audio/"):
		return "tweet_video"
	default:
		return "tweet_image"
	}
}

func (p *Poster) CreateTweet(ctx context.Context, text string, media []MediaRef, replyToTweetID string) (xdk.JSON, error) {
	body := map[string]any{}
	if strings.TrimSpace(text) != "" {
		body["text"] = strings.TrimSpace(text)
	}
	if strings.TrimSpace(replyToTweetID) != "" {
		body["reply"] = map[string]any{
			"in_reply_to_tweet_id": strings.TrimSpace(replyToTweetID),
		}
	}

	mediaIDs := uniqueNonEmpty(mediaIDs(media))
	mediaKeys := uniqueNonEmpty(mediaKeys(media))
	if len(mediaIDs) > 0 {
		body["media"] = map[string]any{
			"media_ids": mediaIDs,
		}
	}

	resp, err := p.client.Posts.Create(ctx, xdk.Params{"body": body})
	if err == nil {
		return resp, nil
	}

	if len(mediaKeys) > 0 {
		body["media"] = map[string]any{
			"media_keys": mediaKeys,
		}
		return p.client.Posts.Create(ctx, xdk.Params{"body": body})
	}

	return nil, err
}

func (p *Poster) GetTimeline(ctx context.Context, params xdk.Params) (xdk.JSON, error) {
	pager := p.client.Users.GetTimeline(params)
	page, ok, err := pager.Next(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return xdk.JSON{"data": []any{}, "meta": map[string]any{"result_count": 0}}, nil
	}
	return page, nil
}

func mediaIDs(items []MediaRef) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.ID) != "" {
			out = append(out, strings.TrimSpace(item.ID))
		}
	}
	return out
}

func mediaKeys(items []MediaRef) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.MediaKey) != "" {
			out = append(out, strings.TrimSpace(item.MediaKey))
		}
	}
	return out
}

func uniqueNonEmpty(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func extractMediaRef(payload any) MediaRef {
	// Keep stable priority: media_id_string > media_id > id.
	id := findFirstByPriority(payload, []string{"media_id_string", "media_id", "id"})
	key := findFirstByPriority(payload, []string{"media_key"})
	return MediaRef{
		ID:       id,
		MediaKey: key,
	}
}

func findFirstByPriority(payload any, keys []string) string {
	switch v := payload.(type) {
	case map[string]any:
		// Check current level by key priority first.
		for _, key := range keys {
			for k, raw := range v {
				if strings.EqualFold(k, key) {
					if s := stringify(raw); s != "" {
						return s
					}
				}
			}
		}
		// Then search nested structures.
		for _, raw := range v {
			if s := findFirstByPriority(raw, keys); s != "" {
				return s
			}
		}
	case []any:
		for _, raw := range v {
			if s := findFirstByPriority(raw, keys); s != "" {
				return s
			}
		}
	}
	return ""
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case fmt.Stringer:
		return strings.TrimSpace(t.String())
	case json.Number:
		return strings.TrimSpace(t.String())
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%v", t)
	case float32:
		f := float64(t)
		if math.Trunc(f) == f {
			return strconv.FormatFloat(f, 'f', 0, 64)
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	case float64:
		if math.Trunc(t) == t {
			return strconv.FormatFloat(t, 'f', 0, 64)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		out = append(out, item)
	}
	return uniqueNonEmpty(out)
}

func effectiveOAuth2Scopes(scopes []string) []string {
	if len(scopes) == 0 {
		return []string{"tweet.read", "tweet.write", "users.read", "offline.access"}
	}
	return uniqueNonEmpty(scopes)
}

func oauth2TokenFromConfig(cfg XAuthConfig) map[string]any {
	token := map[string]any{
		"access_token": strings.TrimSpace(cfg.OAuth2AccessToken),
	}
	if token["access_token"] == "" {
		return nil
	}
	if v := strings.TrimSpace(cfg.OAuth2RefreshToken); v != "" {
		token["refresh_token"] = v
	}
	if v := strings.TrimSpace(cfg.OAuth2TokenType); v != "" {
		token["token_type"] = v
	}
	if v := strings.Join(cfg.OAuth2Scope, " "); strings.TrimSpace(v) != "" {
		token["scope"] = strings.TrimSpace(v)
	}
	if cfg.OAuth2ExpiresAt > 0 {
		token["expires_at"] = cfg.OAuth2ExpiresAt
	}
	return token
}
