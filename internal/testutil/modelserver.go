package testutil

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// RepoRoot returns the repository root (directory holding go.mod).
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// ModelServerURL returns a BentoML model server serving tests/fixtures/model/tiny.onnx for both aliases:
// FRAUD_TEST_MODEL_URL when set (compose.test.yaml), otherwise `uv run bentoml serve` on a free port,
// stopped when the test ends.
func ModelServerURL(t testing.TB) string {
	t.Helper()
	if u := os.Getenv("FRAUD_TEST_MODEL_URL"); u != "" {
		return u
	}
	url, cmd, err := startModelServer()
	if err != nil {
		t.Fatalf("model server unavailable (set FRAUD_TEST_MODEL_URL or install uv): %v", err)
	}
	t.Cleanup(func() { killGroup(cmd) })
	return url
}

func startModelServer() (string, *exec.Cmd, error) {
	root := RepoRoot()
	dir, err := os.MkdirTemp("", "model-fixture-")
	if err != nil {
		return "", nil, err
	}
	onnx, err := os.ReadFile(filepath.Join(root, "tests", "fixtures", "model", "tiny.onnx")) // #nosec G304 -- fixture
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "model.onnx"), onnx, 0o600); err != nil {
		return "", nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	cmd := exec.Command("uv", "run", "--frozen", "python", "-m", "bentoml", "serve", "serving.bentoml.service:FraudModel", // #nosec G204 -- fixed command
		"--host", "127.0.0.1", "--port", fmt.Sprint(port))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "FRAUD_MODEL_DIR="+dir, "BENTOML_DO_NOT_TRACK=true", "FRAUD_MODEL_CACHE="+filepath.Join(dir, "cache"), "MLFLOW_TRACKING_URI=")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(90 * time.Second)
	client := http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url + "/readyz"); err == nil { //nolint:noctx // test helper polling
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return url, cmd, nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	killGroup(cmd)
	return "", nil, fmt.Errorf("bentoml did not become ready on %s", url)
}

func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}
