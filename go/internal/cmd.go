package actions

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
)

const exitUsage = 2

func Execute(args []string) int {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	commands := map[string]func(context.Context) error{
		"mirror":    command(MirrorConfigFromEnv, RunMirror),
		"reconcile": command(ConfigFromEnv, Run),
	}

	if len(args) != 1 {
		return usage(commands)
	}

	run, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])

		return usage(commands)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx)
	if err == nil {
		return 0
	}

	report(args[0], err)

	return 1
}

func command[C any](
	load func() (C, error),
	run func(context.Context, C) error,
) func(context.Context) error {
	return func(ctx context.Context) error {
		config, err := load()
		if err != nil {
			return fmt.Errorf("load configuration: %w", err)
		}

		return run(ctx, config)
	}
}

func usage(commands map[string]func(context.Context) error) int {
	names := slices.Sorted(maps.Keys(commands))
	fmt.Fprintln(os.Stderr, "usage: actions <"+strings.Join(names, "|")+">")

	return exitUsage
}

func report(command string, err error) {
	messages := errorMessages(err)
	if len(messages) == 1 {
		slog.Error("run failed", "command", command, "error", messages[0])

		return
	}

	slog.Error("run failed", "command", command, "errors", len(messages))

	for _, message := range messages {
		slog.Error("run error", "command", command, "error", message)
	}
}

func errorMessages(err error) []string {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []string{oneLine(err.Error())}
	}

	children := joined.Unwrap()
	messages := make([]string, 0, len(children))

	for _, child := range children {
		messages = append(messages, errorMessages(child)...)
	}

	return messages
}

func oneLine(message string) string {
	return strings.Join(strings.Fields(message), " ")
}
