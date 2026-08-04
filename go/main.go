package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rebis-org/actions/internal/reconcile"
)

func main() {
	os.Exit(execute())
}

func execute() int {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config, err := reconcile.ConfigFromEnv()
	if err == nil {
		err = reconcile.Run(ctx, config)
	}

	if err == nil {
		return 0
	}

	errors := errorMessages(err)
	if len(errors) == 1 {
		slog.Error("reconcile failed", "error", errors[0])
	} else {
		slog.Error("reconcile failed", "errors", len(errors))

		for _, message := range errors {
			slog.Error("reconcile error", "error", message)
		}
	}

	return 1
}

func errorMessages(err error) []string {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		messages := make([]string, 0, len(joined.Unwrap()))
		for _, child := range joined.Unwrap() {
			messages = append(messages, errorMessages(child)...)
		}

		return messages
	}

	return []string{strings.Join(strings.Fields(err.Error()), " ")}
}
