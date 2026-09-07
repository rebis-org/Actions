package main

import (
	"context"
	"errors"
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

	messages := errorMessages(err)
	if len(messages) == 1 {
		slog.Error("reconcile failed", "error", messages[0])
	} else {
		slog.Error("reconcile failed", "errors", len(messages))

		for _, message := range messages {
			slog.Error("reconcile error", "error", message)
		}
	}

	return 1
}

type joinError interface {
	error
	Unwrap() []error
}

func errorMessages(err error) []string {
	if joined, ok := errors.AsType[joinError](err); ok {
		children := joined.Unwrap()
		childText := strings.Join(errorTexts(children), "\n")

		if err.Error() != childText {
			return []string{strings.Join(strings.Fields(err.Error()), " ")}
		}

		messages := make([]string, 0, len(children))
		for _, child := range children {
			messages = append(messages, errorMessages(child)...)
		}

		return messages
	}

	return []string{strings.Join(strings.Fields(err.Error()), " ")}
}

func errorTexts(errs []error) []string {
	texts := make([]string, 0, len(errs))
	for _, err := range errs {
		texts = append(texts, err.Error())
	}

	return texts
}
