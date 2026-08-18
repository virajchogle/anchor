// Command anchord serves Anchor's observability panel and runs the reconciler.
//
// One binary, two runtimes. Locally it is an HTTP server; in AWS it is a Lambda
// behind a Function URL. The handler is identical in both, so what the demo
// shows is what runs deployed.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/virajchogle/anchor/internal/bedrock"
	anchorcfg "github.com/virajchogle/anchor/internal/config"
	"github.com/virajchogle/anchor/internal/panel"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()

	anchorcfg.LoadLocalEnv()
	url, err := databaseURL(ctx)
	if err != nil {
		log.Error("resolving database credentials", "error", err)
		os.Exit(1)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		log.Error("parsing database URL", "error", err)
		os.Exit(1)
	}
	// Lambda concurrency is per-instance, so a large pool wastes cluster
	// connections without helping. Keep it small and let Lambda scale out.
	cfg.MaxConns = 4
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Error("connecting to CockroachDB", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	srv := panel.New(pool, log)

	// Live recall needs an embedding model. If Bedrock is unavailable the panel
	// still serves everything else, because an observability page that goes dark
	// when a model is unreachable is worse than one that degrades.
	if emb, err := bedrock.New(ctx, os.Getenv("AWS_REGION")); err != nil {
		log.Warn("live recall disabled, embedder unavailable", "error", err)
	} else {
		srv = srv.WithEmbedder(emb)
		log.Info("live recall enabled", "model", emb.Model())
	}

	handler := srv.Handler()

	if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" {
		log.Info("starting in Lambda mode")
		lambda.Start(functionURLAdapter(handler))
		return
	}

	addr := os.Getenv("ANCHOR_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Info("serving panel", "addr", addr)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// databaseURL resolves the connection string.
//
// Deployed, it comes from AWS Secrets Manager so the credential is never in an
// environment variable visible in the Lambda console, in a task definition, or
// in a process listing. Locally it falls back to ANCHOR_DB_URL, which is read
// from a file outside the repository.
func databaseURL(ctx context.Context) (string, error) {
	if secretID := os.Getenv("ANCHOR_DB_SECRET"); secretID != "" {
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return "", fmt.Errorf("loading AWS config: %w", err)
		}
		out, err := secretsmanager.NewFromConfig(cfg).GetSecretValue(ctx,
			&secretsmanager.GetSecretValueInput{SecretId: &secretID})
		if err != nil {
			return "", fmt.Errorf("reading secret %s: %w", secretID, err)
		}
		if out.SecretString == nil || *out.SecretString == "" {
			return "", fmt.Errorf("secret %s is empty", secretID)
		}
		return *out.SecretString, nil
	}

	if url := os.Getenv("ANCHOR_DB_URL"); url != "" {
		return url, nil
	}
	return "", fmt.Errorf("set ANCHOR_DB_SECRET (deployed) or ANCHOR_DB_URL (local)")
}

// functionURLAdapter bridges a Lambda Function URL event to a net/http handler.
//
// Written by hand rather than pulling in a proxy library: it is thirty lines,
// and a dependency whose only job is translating one struct is a dependency
// whose failure modes are harder to reason about than the translation.
func functionURLAdapter(h http.Handler) func(context.Context, events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	return func(ctx context.Context, ev events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
		path := ev.RawPath
		if path == "" {
			path = "/"
		}
		if ev.RawQueryString != "" {
			path += "?" + ev.RawQueryString
		}

		body := ev.Body
		if ev.IsBase64Encoded {
			if decoded, err := base64.StdEncoding.DecodeString(body); err == nil {
				body = string(decoded)
			}
		}

		method := ev.RequestContext.HTTP.Method
		if method == "" {
			method = http.MethodGet
		}

		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		for k, v := range ev.Headers {
			req.Header.Set(k, v)
		}

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		headers := map[string]string{}
		for k := range rec.Header() {
			headers[k] = rec.Header().Get(k)
		}
		return events.LambdaFunctionURLResponse{
			StatusCode: rec.Code,
			Headers:    headers,
			Body:       rec.Body.String(),
		}, nil
	}
}
