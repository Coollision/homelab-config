package controllers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var authLog = log.Log.WithName("auth-server")

type AuthServer struct {
	Client          client.Client
	TemplateFS      fs.FS
	StackNamespace  string
	StackConfigName string

	ssoSessions sync.Map // sid -> *ssoSession
	tmplOnce    sync.Once
	tmplErr     error
	templates   map[string]*template.Template
}

type exportCredentials struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration"`
}

type loginRequest struct {
	Namespace string `json:"namespace"`
	Stack     string `json:"stack"`
	Profile   string `json:"profile"`
}

type loginTarget struct {
	Namespace  string `json:"namespace"`
	Stack      string `json:"stack"`
	Profile    string `json:"profile"`
	Secret     string `json:"secret"`
	Expiration string `json:"expiration,omitempty"`
	Valid      bool   `json:"valid"`
}

type ssoSession struct {
	Profile    string
	Namespace  string
	Stack      string
	mu         sync.Mutex
	url        string
	code       string
	done       chan struct{}
	loginErr   error
	restarted  int
	expiration string
}

func (s *ssoSession) setURLCode(u, c string) {
	s.mu.Lock()
	if s.url == "" && u != "" {
		s.url = u
	}
	if s.code == "" && c != "" {
		s.code = c
	}
	s.mu.Unlock()
}

func (s *ssoSession) snapshot() (ssoURL, code string, isDone bool, loginErr error, restarted int, expiration string) {
	s.mu.Lock()
	ssoURL, code, loginErr, restarted, expiration = s.url, s.code, s.loginErr, s.restarted, s.expiration
	s.mu.Unlock()
	select {
	case <-s.done:
		isDone = true
	default:
	}
	return
}

type authRootPageData struct {
	Targets []loginTarget
	Msg     string
	Err     string
}

type authSSOWaitPageData struct {
	SessionID string
	Stack     string
	Profile   string
	URL       string
	Code      string
}

func (s *AuthServer) loadTemplates() error {
	s.tmplOnce.Do(func() {
		tmpl, err := template.ParseFS(s.TemplateFS, "templates/auth-root.html", "templates/auth-sso-wait.html")
		if err != nil {
			s.tmplErr = fmt.Errorf("loading templates: %w", err)
			return
		}
		s.templates = map[string]*template.Template{
			"auth-root":     tmpl.Lookup("auth-root.html"),
			"auth-sso-wait": tmpl.Lookup("auth-sso-wait.html"),
		}
	})
	return s.tmplErr
}

func (s *AuthServer) Register(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/status.json", s.handleStatus)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/login-wait", s.handleLoginWait)
	mux.HandleFunc("/login-poll.json", s.handleLoginPoll)
	mux.HandleFunc("/restart", s.handleRestart)
}

func (s *AuthServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := s.loadTemplates(); err != nil {
		authLog.Error(err, "failed to load templates")
		http.Error(w, fmt.Sprintf("failed to load templates: %v", err), http.StatusInternalServerError)
		return
	}

	targets, err := s.discoverTargets(r.Context())
	if err != nil {
		authLog.Error(err, "failed to discover login targets")
		http.Error(w, fmt.Sprintf("failed to load targets: %v", err), http.StatusInternalServerError)
		return
	}

	data := authRootPageData{
		Targets: targets,
		Msg:     r.URL.Query().Get("msg"),
		Err:     r.URL.Query().Get("err"),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates["auth-root"].Execute(w, data); err != nil {
		authLog.Error(err, "failed to render auth-root template")
		http.Error(w, fmt.Sprintf("failed to render page: %v", err), http.StatusInternalServerError)
	}
}

func (s *AuthServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	targets, err := s.discoverTargets(r.Context())
	if err != nil {
		authLog.Error(err, "failed to discover targets in status")
		http.Error(w, fmt.Sprintf("failed to load status: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"targets": targets})
}

func (s *AuthServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	req, formMode, err := decodeLoginRequest(r)
	if err != nil {
		s.redirectOrError(w, r, formMode, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Profile == "" {
		s.redirectOrError(w, r, formMode, "profile is required", http.StatusBadRequest)
		return
	}

	cfg, err := LoadStackConfig(r.Context(), s.Client, s.StackNamespace, s.StackConfigName)
	if err != nil {
		authLog.Error(err, "failed to load stack config for login", "profile", req.Profile)
		s.redirectOrError(w, r, formMode, fmt.Sprintf("failed to load stack config: %v", err), http.StatusInternalServerError)
		return
	}
	if req.Namespace == "" {
		req.Namespace = cfg.Namespace
	}
	if req.Stack == "" {
		req.Stack = cfg.Name
	}

	authLog.Info("login requested", "profile", req.Profile, "namespace", req.Namespace, "stack", req.Stack, "formMode", formMode)
	_, awsConfigText, awsProfiles, err := SyncAWSConfigMap(r.Context(), s.Client, cfg)
	if err != nil {
		authLog.Error(err, "failed to sync aws config configmap for login", "profile", req.Profile)
		s.redirectOrError(w, r, formMode, fmt.Sprintf("failed to build aws config: %v", err), http.StatusBadRequest)
		return
	}

	if formMode {
		sid := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
		sess := &ssoSession{Profile: req.Profile, Namespace: req.Namespace, Stack: req.Stack, done: make(chan struct{})}
		s.ssoSessions.Store(sid, sess)
		authLog.Info("started async sso session", "sid", sid, "profile", req.Profile)
		go s.runSSOLogin(req, sess, awsConfigText, awsProfiles)
		http.Redirect(w, r, "/login-wait?sid="+url.QueryEscape(sid), http.StatusSeeOther)
		return
	}

	creds, err := s.loginAndExport(req.Profile, awsConfigText, awsProfiles)
	if err != nil {
		authLog.Error(err, "sync login failed", "profile", req.Profile)
		s.redirectOrError(w, r, formMode, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.storeCredentials(context.Background(), req, creds); err != nil {
		authLog.Error(err, "failed storing credentials", "profile", req.Profile)
		s.redirectOrError(w, r, formMode, err.Error(), http.StatusInternalServerError)
		return
	}
	restarted, err := s.restartStackTunnels(context.Background(), req.Namespace, req.Stack, req.Profile)
	if err != nil {
		authLog.Error(err, "failed restart after sync login", "profile", req.Profile)
		s.redirectOrError(w, r, formMode, fmt.Sprintf("credentials updated, but failed to restart tunnels: %v", err), http.StatusInternalServerError)
		return
	}
	message := fmt.Sprintf("credentials updated; restarted %d matching tunnel deployment(s)", restarted)
	authLog.Info("sync login complete", "profile", req.Profile, "restarted", restarted)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"secret":     credsSecretName(req.Stack, req.Profile),
		"profile":    req.Profile,
		"expiration": creds.Expiration,
		"restarted":  restarted,
		"message":    message,
	})
}

func (s *AuthServer) handleLoginWait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.loadTemplates(); err != nil {
		http.Error(w, fmt.Sprintf("failed to load templates: %v", err), http.StatusInternalServerError)
		return
	}

	sid := strings.TrimSpace(r.URL.Query().Get("sid"))
	if sid == "" {
		http.Error(w, "missing sid", http.StatusBadRequest)
		return
	}

	ssoURL := ""
	code := ""
	profile := ""
	stack := ""
	if val, ok := s.ssoSessions.Load(sid); ok {
		sess := val.(*ssoSession)
		ssoURL, code, _, _, _, _ = sess.snapshot()
		profile = sess.Profile
		stack = sess.Stack
	}

	data := authSSOWaitPageData{SessionID: sid, Stack: stack, Profile: profile, URL: ssoURL, Code: code}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates["auth-sso-wait"].Execute(w, data); err != nil {
		http.Error(w, fmt.Sprintf("failed to render page: %v", err), http.StatusInternalServerError)
	}
}

func (s *AuthServer) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sid := strings.TrimSpace(r.URL.Query().Get("sid"))
	w.Header().Set("Content-Type", "application/json")
	if sid == "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "missing sid"})
		return
	}

	val, ok := s.ssoSessions.Load(sid)
	if !ok {
		authLog.Info("poll for unknown sid", "sid", sid)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "login session not found or expired"})
		return
	}

	ssoURL, code, isDone, loginErr, restarted, expiration := val.(*ssoSession).snapshot()
	if isDone {
		s.ssoSessions.Delete(sid)
		if loginErr != nil {
			authLog.Error(loginErr, "async login failed", "sid", sid)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": loginErr.Error()})
			return
		}
		authLog.Info("async login complete", "sid", sid, "restarted", restarted)
		msg := fmt.Sprintf("credentials updated (expires %s); restarted %d tunnel(s)", expiration, restarted)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "done", "redirect": "/?msg=" + url.QueryEscape(msg)})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "waiting", "url": ssoURL, "code": code})
}

func (s *AuthServer) runSSOLogin(req loginRequest, sess *ssoSession, awsConfigText string, awsProfiles []string) {
	defer close(sess.done)
	authLog.Info("running async aws sso login", "profile", req.Profile, "stack", req.Stack)

	awsEnv, cleanupAWSConfig, err := buildAWSCLIEnv(req.Profile, awsConfigText, awsProfiles)
	if err != nil {
		sess.mu.Lock()
		sess.loginErr = err
		sess.mu.Unlock()
		authLog.Error(err, "failed to prepare aws cli config", "profile", req.Profile)
		return
	}
	defer cleanupAWSConfig()

	cmd := exec.Command("aws", "sso", "login", "--profile", req.Profile, "--no-browser")
	cmd.Env = append(os.Environ(), awsEnv...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pw.CloseWithError(err)
		sess.mu.Lock()
		sess.loginErr = fmt.Errorf("failed to start aws sso login: %w", err)
		sess.mu.Unlock()
		authLog.Error(err, "failed to start aws sso login", "profile", req.Profile)
		return
	}

	urlRe := regexp.MustCompile(`https://[^\s]+`)
	codeRe := regexp.MustCompile(`\b[A-Z0-9]{4}-[A-Z0-9]{4}\b`)

	outputDone := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(pr)
		var all strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			all.WriteString(line + "\n")
			urlMatch := urlRe.FindString(line)
			codeMatch := codeRe.FindString(line)
			sess.setURLCode(urlMatch, codeMatch)
			if urlMatch != "" {
				authLog.Info("discovered sso URL", "profile", req.Profile)
			}
		}
		outputDone <- all.String()
	}()

	cmdErr := cmd.Wait()
	_ = pw.Close()
	rawOutput := <-outputDone
	if cmdErr != nil {
		sess.mu.Lock()
		sess.loginErr = fmt.Errorf("aws sso login failed: %w\n%s", cmdErr, rawOutput)
		sess.mu.Unlock()
		authLog.Error(cmdErr, "aws sso login command failed", "profile", req.Profile, "output", clipLogOutput(rawOutput))
		return
	}

	creds, err := s.exportCredentials(req.Profile, awsEnv)
	if err != nil {
		sess.mu.Lock()
		sess.loginErr = err
		sess.mu.Unlock()
		authLog.Error(err, "failed to export credentials", "profile", req.Profile)
		return
	}
	if err := s.storeCredentials(context.Background(), req, creds); err != nil {
		sess.mu.Lock()
		sess.loginErr = err
		sess.mu.Unlock()
		authLog.Error(err, "failed to store credentials", "profile", req.Profile)
		return
	}

	restarted, err := s.restartStackTunnels(context.Background(), req.Namespace, req.Stack, req.Profile)
	if err != nil {
		sess.mu.Lock()
		sess.loginErr = fmt.Errorf("credentials stored, but failed to restart tunnels: %w", err)
		sess.mu.Unlock()
		authLog.Error(err, "failed restarting tunnels after async login", "profile", req.Profile)
		return
	}

	sess.mu.Lock()
	sess.restarted = restarted
	sess.expiration = creds.Expiration
	sess.mu.Unlock()
	authLog.Info("async login flow completed", "profile", req.Profile, "stack", req.Stack, "restarted", restarted)
}

func (s *AuthServer) loginAndExport(profile string, awsConfigText string, awsProfiles []string) (exportCredentials, error) {
	awsEnv, cleanupAWSConfig, err := buildAWSCLIEnv(profile, awsConfigText, awsProfiles)
	if err != nil {
		return exportCredentials{}, err
	}
	defer cleanupAWSConfig()

	loginCmd := exec.Command("aws", "sso", "login", "--profile", profile, "--no-browser")
	loginCmd.Env = append(os.Environ(), awsEnv...)
	loginOut, err := loginCmd.CombinedOutput()
	if err != nil {
		authLog.Error(err, "sync aws sso login command failed", "profile", profile, "output", clipLogOutput(string(loginOut)))
		return exportCredentials{}, fmt.Errorf("aws sso login failed: %v\n%s", err, string(loginOut))
	}
	return s.exportCredentials(profile, awsEnv)
}

func buildAWSCLIEnv(targetProfile string, awsConfigText string, availableProfiles []string) ([]string, func(), error) {
	targetProfile = strings.TrimSpace(targetProfile)
	if targetProfile == "" {
		return nil, nil, fmt.Errorf("aws profile is required")
	}
	if strings.TrimSpace(awsConfigText) == "" {
		return nil, nil, fmt.Errorf("aws config is empty")
	}

	found := false
	for _, p := range availableProfiles {
		if p == targetProfile {
			found = true
			break
		}
	}
	if !found {
		return nil, nil, fmt.Errorf("aws profile %q not found in stack config. Available profiles: %s", targetProfile, strings.Join(availableProfiles, ", "))
	}

	tmp, err := os.CreateTemp("", "aws-config-*.ini")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp aws config file: %w", err)
	}
	if _, err := tmp.WriteString(awsConfigText); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, nil, fmt.Errorf("write temp aws config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, nil, fmt.Errorf("close temp aws config file: %w", err)
	}

	cleanup := func() {
		_ = os.Remove(tmp.Name())
	}

	env := []string{
		"AWS_CONFIG_FILE=" + tmp.Name(),
		"AWS_SDK_LOAD_CONFIG=1",
	}
	return env, cleanup, nil
}

func clipLogOutput(s string) string {
	s = strings.TrimSpace(s)
	const maxLen = 600
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func (s *AuthServer) exportCredentials(profile string, awsEnv []string) (exportCredentials, error) {
	exportCmd := exec.Command("aws", "configure", "export-credentials", "--profile", profile, "--format", "process")
	exportCmd.Env = append(os.Environ(), awsEnv...)
	exportOut, err := exportCmd.CombinedOutput()
	if err != nil {
		return exportCredentials{}, fmt.Errorf("aws configure export-credentials failed: %v\n%s", err, string(exportOut))
	}
	var creds exportCredentials
	if err := json.Unmarshal(exportOut, &creds); err != nil {
		return exportCredentials{}, fmt.Errorf("failed to parse exported credentials: %w", err)
	}
	if strings.TrimSpace(creds.Expiration) == "" {
		creds.Expiration = time.Now().UTC().Add(45 * time.Minute).Format(time.RFC3339)
	}
	return creds, nil
}

func (s *AuthServer) storeCredentials(ctx context.Context, req loginRequest, creds exportCredentials) error {
	secretName := credsSecretName(req.Stack, req.Profile)
	secret := &corev1.Secret{}
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: secretName}, secret)
	if err != nil {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: req.Namespace}}
	}
	secret.Type = corev1.SecretTypeOpaque
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data["AWS_ACCESS_KEY_ID"] = []byte(creds.AccessKeyID)
	secret.Data["AWS_SECRET_ACCESS_KEY"] = []byte(creds.SecretAccessKey)
	secret.Data["AWS_SESSION_TOKEN"] = []byte(creds.SessionToken)
	secret.Data["expiration"] = []byte(creds.Expiration)
	if secret.UID == "" {
		return s.Client.Create(ctx, secret)
	}
	return s.Client.Update(ctx, secret)
}

func (s *AuthServer) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	formMode := isFormPost(r)
	namespace := strings.TrimSpace(r.FormValue("namespace"))
	stack := strings.TrimSpace(r.FormValue("stack"))
	if !formMode {
		var payload struct {
			Namespace string `json:"namespace"`
			Stack     string `json:"stack"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			if namespace == "" {
				namespace = strings.TrimSpace(payload.Namespace)
			}
			if stack == "" {
				stack = strings.TrimSpace(payload.Stack)
			}
		}
	}

	if namespace == "" {
		namespace = s.StackNamespace
	}
	if stack == "" {
		if cfg, err := LoadStackConfig(r.Context(), s.Client, namespace, s.StackConfigName); err == nil {
			stack = cfg.Name
		}
	}
	if namespace == "" || stack == "" {
		s.redirectOrError(w, r, formMode, "namespace and stack are required", http.StatusBadRequest)
		return
	}

	authLog.Info("restart requested", "namespace", namespace, "stack", stack, "formMode", formMode)
	restarted, err := s.restartStackTunnels(r.Context(), namespace, stack, "")
	if err != nil {
		authLog.Error(err, "restart request failed", "namespace", namespace, "stack", stack)
		s.redirectOrError(w, r, formMode, fmt.Sprintf("failed to restart tunnels: %v", err), http.StatusInternalServerError)
		return
	}
	message := fmt.Sprintf("restarted %d tunnel deployment(s)", restarted)
	if formMode {
		http.Redirect(w, r, "/?msg="+url.QueryEscape(message), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "restarted": restarted, "message": message})
}

func decodeLoginRequest(r *http.Request) (loginRequest, bool, error) {
	if isFormPost(r) {
		if err := r.ParseForm(); err != nil {
			return loginRequest{}, true, err
		}
		return loginRequest{
			Namespace: strings.TrimSpace(r.FormValue("namespace")),
			Stack:     strings.TrimSpace(r.FormValue("stack")),
			Profile:   strings.TrimSpace(r.FormValue("profile")),
		}, true, nil
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return loginRequest{}, false, err
	}
	req.Namespace = strings.TrimSpace(req.Namespace)
	req.Stack = strings.TrimSpace(req.Stack)
	req.Profile = strings.TrimSpace(req.Profile)
	return req, false, nil
}

func isFormPost(r *http.Request) bool {
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.Contains(contentType, "application/x-www-form-urlencoded") || strings.Contains(contentType, "multipart/form-data")
}

func (s *AuthServer) redirectOrError(w http.ResponseWriter, r *http.Request, formMode bool, msg string, code int) {
	if formMode {
		http.Redirect(w, r, "/?err="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	http.Error(w, msg, code)
}

func (s *AuthServer) discoverTargets(ctx context.Context) ([]loginTarget, error) {
	cfg, err := LoadStackConfig(ctx, s.Client, s.StackNamespace, s.StackConfigName)
	if err != nil {
		return nil, err
	}

	profiles := cfg.ReferencedAWSProfiles()
	targets := make([]loginTarget, 0, len(profiles))
	for _, profile := range profiles {
		secretName := credsSecretName(cfg.Name, profile)
		t := loginTarget{Namespace: cfg.Namespace, Stack: cfg.Name, Profile: profile, Secret: secretName}
		secret := &corev1.Secret{}
		if err := s.Client.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: secretName}, secret); err == nil {
			t.Valid = IsCredentialValid(secret)
			if secret.Data != nil {
				t.Expiration = strings.TrimSpace(string(secret.Data["expiration"]))
			}
		}
		targets = append(targets, t)
	}

	sort.Slice(targets, func(i, j int) bool {
		a, b := targets[i], targets[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Stack != b.Stack {
			return a.Stack < b.Stack
		}
		return a.Profile < b.Profile
	})

	return targets, nil
}

func (s *AuthServer) restartStackTunnels(ctx context.Context, namespace, stackName, profile string) (int, error) {
	cfg, err := LoadStackConfig(ctx, s.Client, namespace, s.StackConfigName)
	if err != nil {
		return 0, err
	}
	if stackName != "" && cfg.Name != stackName {
		return 0, fmt.Errorf("stack %q not configured", stackName)
	}

	restarted := 0
	restartAt := time.Now().UTC().Format(time.RFC3339)
	for _, tunnel := range cfg.Spec.Tunnels {
		tunnelProfile := tunnel.AWSProfile
		if tunnelProfile == "" {
			tunnelProfile = cfg.Spec.AWS.Profile
		}
		if profile != "" && tunnelProfile != profile {
			continue
		}

		depName := fmt.Sprintf("%s-%s", cfg.Name, tunnel.Name)
		dep := &appsv1.Deployment{}
		if err := s.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: depName}, dep); err != nil {
			continue
		}
		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		dep.Spec.Template.Annotations["proxies.homelab.io/restartedAt"] = restartAt
		if err := s.Client.Update(ctx, dep); err != nil {
			return restarted, err
		}
		restarted++
	}
	return restarted, nil
}
