package serve

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Governing: ADR-0031 (One Cairn Binary — The Server Is `cairn serve`),
// SPEC-0002 (the /healthz contract).
//
// The startup pin: `cairn serve` must start, serve, and shut down on the
// CAIRN_* environment alone — the contract the former `cairnd` binary had.
// Needs a real Postgres (migrations run at startup) and an S3-compatible
// store, so it self-skips without the integration environment the CI
// integration job provides; the same gates the store/webhook harnesses use.
func TestServeStartsServesHealthzAndStopsOnCancel(t *testing.T) {
	dsn := os.Getenv("CAIRN_TEST_DATABASE_URL")
	s3 := os.Getenv("CAIRN_TEST_S3_ENDPOINT")
	if dsn == "" || s3 == "" {
		t.Skip("integration: CAIRN_TEST_DATABASE_URL and CAIRN_TEST_S3_ENDPOINT required")
	}

	// A free port, so the test never races another suite for CAIRN_HTTP_ADDR.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not bind a probe listener: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	t.Setenv("CAIRN_HTTP_ADDR", addr)
	t.Setenv("CAIRN_DATABASE_URL", dsn)
	t.Setenv("CAIRN_S3_ENDPOINT", s3)
	t.Setenv("CAIRN_S3_ACCESS_KEY", os.Getenv("CAIRN_TEST_S3_ACCESS_KEY"))
	t.Setenv("CAIRN_S3_SECRET_KEY", os.Getenv("CAIRN_TEST_S3_SECRET_KEY"))
	t.Setenv("CAIRN_S3_BUCKET", "cairn")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logBuf strings.Builder
	logger := slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		logBuf.Write(p)
		return len(p), nil
	}), nil))

	done := make(chan error, 1)
	go func() { done <- Run(ctx, logger) }()

	// Poll healthz until the server is up (or the run fails early).
	client := &http.Client{Timeout: 2 * time.Second}
	healthy := false
	for i := 0; i < 120; i++ {
		select {
		case err := <-done:
			t.Fatalf("Run returned before shutdown: %v\nlogs:\n%s", err, logBuf.String())
		default:
		}
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "ok" {
				healthy = true
				break
			}
			t.Fatalf("healthz = %d %q, want 200 ok", resp.StatusCode, body)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("server never became healthy on %s\nlogs:\n%s", addr, logBuf.String())
	}

	// The startup line names the new invocation, so log-grepping operators can
	// tell the folded binary from the old one (self-hosting guide pins this).
	if !strings.Contains(logBuf.String(), "cairn serve listening") {
		t.Errorf("startup log does not say \"cairn serve listening\":\n%s", logBuf.String())
	}
	if raw := logLine(&logBuf, "cairn serve listening"); raw != "" {
		var entry struct {
			Addr string `json:"addr"`
		}
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Errorf("startup line is not the structured JSON the guide shows: %v", err)
		} else if entry.Addr != addr {
			t.Errorf("startup line addr = %q, want %q (CAIRN_HTTP_ADDR must be honored)", entry.Addr, addr)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancel = %v, want a graceful nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s of context cancellation")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func logLine(buf *strings.Builder, contains string) string {
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, contains) {
			return line
		}
	}
	return ""
}
