package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	xdk "github.com/missuo/xdk-go"
)

func RunCLI(args []string) error {
	if len(args) == 0 {
		return RunLocal()
	}

	switch args[0] {
	case "serve":
		return RunLocal()
	case "login":
		return runLoginCommand(args[1:])
	case "tweet":
		return runTweetCommand(args[1:])
	case "install":
		return runInstallCommand(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", args[0])
	}
}

func printUsage() {
	fmt.Println(`xpost commands:
  xpost serve
  xpost login [--client-id ... --redirect-uri ... --scope tweet.read,tweet.write,users.read,offline.access]
  xpost tweet --text "hello" [--media ./image.jpg]
  xpost install [--bin /path/to/xpost --user nobody --dry-run]

if no command is specified, xpost starts HTTP server mode (same as "xpost serve").`)
}

func runLoginCommand(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	clientID := fs.String("client-id", "", "OAuth2 client ID (or X_OAUTH2_CLIENT_ID)")
	clientSecret := fs.String("client-secret", "", "OAuth2 client secret (or X_OAUTH2_CLIENT_SECRET)")
	redirectURI := fs.String("redirect-uri", "", "OAuth2 redirect URI (or X_OAUTH2_REDIRECT_URI)")
	scopeCSV := fs.String("scope", "", "OAuth2 scopes, comma-separated")
	noOpen := fs.Bool("no-open", false, "Do not auto-open browser")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	cfg, configPath, err := loadCLIConfig()
	if err != nil {
		return err
	}

	if strings.TrimSpace(*clientID) != "" {
		cfg.X.OAuth2ClientID = strings.TrimSpace(*clientID)
	}
	if strings.TrimSpace(*clientSecret) != "" {
		cfg.X.OAuth2ClientSecret = strings.TrimSpace(*clientSecret)
	}
	if strings.TrimSpace(*redirectURI) != "" {
		cfg.X.OAuth2RedirectURI = strings.TrimSpace(*redirectURI)
	}
	if strings.TrimSpace(*scopeCSV) != "" {
		cfg.X.OAuth2Scope = splitCSV(*scopeCSV)
	}

	if strings.TrimSpace(cfg.X.OAuth2ClientID) == "" {
		return errors.New("oauth2 client id is required (set --client-id or X_OAUTH2_CLIENT_ID)")
	}
	if strings.TrimSpace(cfg.X.OAuth2RedirectURI) == "" {
		cfg.X.OAuth2RedirectURI = defaultRedirectURI
		fmt.Printf("Using default redirect URI: %s\n", defaultRedirectURI)
		fmt.Println("Make sure this URI is added to your app's callback URLs in the X Developer Portal.")
	}

	scopes := effectiveOAuth2Scopes(cfg.X.OAuth2Scope)
	client := xdk.NewClient(xdk.Config{
		ClientID:     cfg.X.OAuth2ClientID,
		ClientSecret: cfg.X.OAuth2ClientSecret,
		RedirectURI:  cfg.X.OAuth2RedirectURI,
		Scope:        scopes,
	})

	authURL, err := client.GetAuthorizationURL(generateToken())
	if err != nil {
		return fmt.Errorf("failed to generate authorization URL: %w", err)
	}

	fmt.Printf("Open this URL to authorize:\n%s\n\n", authURL)
	if !*noOpen {
		if err := openBrowser(authURL); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to open browser automatically: %v\n", err)
		}
	}

	fmt.Print("Paste callback URL: ")
	reader := bufio.NewReader(os.Stdin)
	callbackURL, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	callbackURL = strings.TrimSpace(callbackURL)
	if callbackURL == "" {
		return errors.New("callback URL cannot be empty")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	token, err := client.FetchToken(ctx, callbackURL)
	if err != nil {
		return fmt.Errorf("oauth2 token exchange failed: %w", err)
	}

	cfg.X.OAuth2Scope = scopes
	if err := applyOAuth2TokenToConfig(&cfg.X, token); err != nil {
		return err
	}
	if err := saveConfig(configPath, cfg); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("Login succeeded. OAuth2 token saved to %s\n", configPath)
	return nil
}

func runTweetCommand(args []string) error {
	fs := flag.NewFlagSet("tweet", flag.ContinueOnError)
	text := fs.String("text", "", "Tweet text")
	var mediaFiles stringSliceFlag
	fs.Var(&mediaFiles, "media", "Media file path (repeatable, max 4)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	tweetText := strings.TrimSpace(*text)
	if tweetText == "" && fs.NArg() > 0 {
		tweetText = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}

	mediaInputs, err := mediaInputsFromPaths(mediaFiles)
	if err != nil {
		return err
	}
	if tweetText == "" && len(mediaInputs) == 0 {
		return errors.New("text or media is required")
	}

	cfg, configPath, err := loadCLIConfig()
	if err != nil {
		return err
	}

	poster, err := newPoster(cfg.X)
	if err != nil {
		return fmt.Errorf("x auth is not ready: %w (run `xpost login` for oauth2)", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	uploaded := make([]MediaRef, 0, len(mediaInputs))
	for _, input := range mediaInputs {
		ref, err := poster.UploadMedia(ctx, input.Data, input.ContentType)
		if err != nil {
			return err
		}
		uploaded = append(uploaded, ref)
	}

	tweetResp, err := poster.CreateTweet(ctx, tweetText, uploaded, "")
	if err != nil {
		return err
	}

	if err := persistOAuth2TokenIfAvailable(cfg, configPath, poster.client); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to persist refreshed oauth2 token: %v\n", err)
	}

	out := map[string]any{
		"ok":          true,
		"auth_mode":   poster.authMode,
		"media_count": len(uploaded),
		"media":       uploaded,
		"tweet":       tweetResp,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func runInstallCommand(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	binPath := fs.String("bin", "", "xpost binary path (default: current executable)")
	runUser := fs.String("user", "", "systemd User (default: caller of sudo)")
	dryRun := fs.Bool("dry-run", false, "print service file without installing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if runtime.GOOS != "linux" {
		return errors.New("install command is only supported on Linux")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl not found in PATH")
	}

	cfg, cfgPath, err := loadCLIConfig()
	if err != nil {
		return err
	}
	if err := ensureFirstBootAuthConfigured(cfg.X); err != nil {
		return fmt.Errorf("credentials not configured: %w\nrun `xpost login` first", err)
	}

	cfgPathAbs, err := filepath.Abs(cfgPath)
	if err != nil {
		return fmt.Errorf("failed to resolve config path: %w", err)
	}

	execPath := strings.TrimSpace(*binPath)
	if execPath == "" {
		current, err := os.Executable()
		if err != nil {
			return fmt.Errorf("failed to detect current executable: %w", err)
		}
		execPath = current
	}
	execPathAbs, err := filepath.Abs(execPath)
	if err != nil {
		return fmt.Errorf("failed to resolve binary path: %w", err)
	}

	userName := strings.TrimSpace(*runUser)
	if userName == "" {
		if sudoUser := strings.TrimSpace(os.Getenv("SUDO_USER")); sudoUser != "" {
			userName = sudoUser
		} else {
			currentUser, err := user.Current()
			if err == nil {
				userName = strings.TrimSpace(currentUser.Username)
			}
		}
	}

	const serviceName = "xpost"
	workDir := filepath.Dir(execPathAbs)
	unitContent := buildSystemdUnit(serviceName, execPathAbs, cfgPathAbs, workDir, userName)
	unitPath := "/etc/systemd/system/" + serviceName + ".service"

	if *dryRun {
		fmt.Printf("# %s\n%s", unitPath, unitContent)
		return nil
	}

	if err := os.WriteFile(unitPath, []byte(unitContent), 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w (hint: run with sudo)", unitPath, err)
	}

	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", serviceName+".service"); err != nil {
		return err
	}

	fmt.Printf("Installed and started %s.service\n", serviceName)
	fmt.Printf("Config: %s\n", cfgPathAbs)
	return nil
}

func loadCLIConfig() (*Config, string, error) {
	configPath := os.Getenv("XPOST_CONFIG")
	if strings.TrimSpace(configPath) == "" {
		configPath = defaultConfigPath()
	}

	cfg, _, err := loadOrInitConfig(configPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to load config: %w", err)
	}
	overrideConfigFromEnv(cfg)
	return cfg, configPath, nil
}

func persistOAuth2TokenIfAvailable(cfg *Config, configPath string, client *xdk.Client) error {
	if cfg == nil || client == nil || client.OAuth2Auth == nil {
		return nil
	}
	token := client.OAuth2Token()
	if len(token) == 0 {
		return nil
	}
	if err := applyOAuth2TokenToConfig(&cfg.X, token); err != nil {
		return nil
	}
	return saveConfig(configPath, cfg)
}

func applyOAuth2TokenToConfig(cfg *XAuthConfig, token map[string]any) error {
	access := stringify(token["access_token"])
	if strings.TrimSpace(access) == "" {
		return errors.New("oauth2 token does not contain access_token")
	}
	cfg.OAuth2AccessToken = strings.TrimSpace(access)
	cfg.OAuth2RefreshToken = strings.TrimSpace(stringify(token["refresh_token"]))
	cfg.OAuth2TokenType = strings.TrimSpace(stringify(token["token_type"]))

	scopeValue := strings.TrimSpace(stringify(token["scope"]))
	if scopeValue != "" {
		if strings.Contains(scopeValue, ",") {
			cfg.OAuth2Scope = splitCSV(scopeValue)
		} else {
			cfg.OAuth2Scope = uniqueNonEmpty(strings.Fields(scopeValue))
		}
	}

	expiresAt, ok := toInt64(token["expires_at"])
	if !ok {
		if expiresIn, ok := toInt64(token["expires_in"]); ok && expiresIn > 0 {
			expiresAt = time.Now().Unix() + expiresIn
		}
	}
	if expiresAt > 0 {
		cfg.OAuth2ExpiresAt = expiresAt
	}

	return nil
}

func toInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int8:
		return int64(t), true
	case int16:
		return int64(t), true
	case int32:
		return int64(t), true
	case int64:
		return t, true
	case uint:
		return int64(t), true
	case uint8:
		return int64(t), true
	case uint16:
		return int64(t), true
	case uint32:
		return int64(t), true
	case uint64:
		if t > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(t), true
	case float32:
		return int64(t), true
	case float64:
		return int64(t), true
	case string:
		if strings.TrimSpace(t) == "" {
			return 0, false
		}
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err == nil {
			return n, true
		}
		return 0, false
	default:
		return 0, false
	}
}

func mediaInputsFromPaths(paths []string) ([]mediaUploadInput, error) {
	if len(paths) > maxMediaCount {
		return nil, fmt.Errorf("too many media files, max is %d", maxMediaCount)
	}

	media := make([]mediaUploadInput, 0, len(paths))
	for _, p := range paths {
		path := filepath.Clean(strings.TrimSpace(p))
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read media file %q: %w", path, err)
		}
		if int64(len(data)) > maxMediaBytes {
			return nil, fmt.Errorf("file %q exceeds max size %d bytes", path, maxMediaBytes)
		}
		media = append(media, mediaUploadInput{
			Data:        data,
			ContentType: http.DetectContentType(data),
		})
	}
	return media, nil
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func buildSystemdUnit(serviceName, execPath, configPath, workDir, runUser string) string {
	lines := []string{
		"[Unit]",
		"Description=" + serviceName + " service",
		"After=network.target",
		"",
		"[Service]",
		"Type=simple",
		"WorkingDirectory=" + workDir,
		"Environment=XPOST_CONFIG=" + configPath,
		"ExecStart=" + execPath + " serve",
		"Restart=always",
		"RestartSec=5",
	}
	if strings.TrimSpace(runUser) != "" {
		lines = append(lines, "User="+strings.TrimSpace(runUser))
	}
	lines = append(lines,
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	)
	return strings.Join(lines, "\n")
}

func runSystemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("systemctl %s failed: %s", strings.Join(args, " "), msg)
	}
	return nil
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(value string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	*s = append(*s, v)
	return nil
}
