package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type AuthServer struct {
	Client client.Client
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

func (s *AuthServer) Register(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/login", s.handleLogin)
}

func (s *AuthServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Namespace == "" || req.Stack == "" || req.Profile == "" {
		http.Error(w, "namespace, stack and profile are required", http.StatusBadRequest)
		return
	}

	loginCmd := exec.Command("aws", "sso", "login", "--profile", req.Profile, "--no-browser")
	loginOut, err := loginCmd.CombinedOutput()
	if err != nil {
		http.Error(w, fmt.Sprintf("aws sso login failed: %v\n%s", err, string(loginOut)), http.StatusBadRequest)
		return
	}

	exportCmd := exec.Command("aws", "configure", "export-credentials", "--profile", req.Profile, "--format", "process")
	exportOut, err := exportCmd.CombinedOutput()
	if err != nil {
		http.Error(w, fmt.Sprintf("aws configure export-credentials failed: %v\n%s", err, string(exportOut)), http.StatusBadRequest)
		return
	}

	var creds exportCredentials
	if err := json.Unmarshal(exportOut, &creds); err != nil {
		http.Error(w, fmt.Sprintf("failed to parse exported credentials: %v", err), http.StatusInternalServerError)
		return
	}

	if strings.TrimSpace(creds.Expiration) == "" {
		creds.Expiration = time.Now().UTC().Add(45 * time.Minute).Format(time.RFC3339)
	}

	secretName := credsSecretName(req.Stack, req.Profile)
	ctx := context.Background()

	secret := &corev1.Secret{}
	err = s.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: secretName}, secret)
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
		err = s.Client.Create(ctx, secret)
	} else {
		err = s.Client.Update(ctx, secret)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to persist credentials secret: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"secret":     secretName,
		"profile":    req.Profile,
		"expiration": creds.Expiration,
		"message":    "credentials updated",
	})
}
