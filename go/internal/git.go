package actions

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	gitHubUsername        = "x-access-token"
	gitConfigBaseKeys     = 4
	maxEnvironmentEntries = 2
	secretPairLength      = 2
	gitHTTPPostBuffer     = 157286400
)

type entry struct {
	Key   string
	Value string
}

func authorization(endpoint, value string) entry {
	return entry{
		Key:   "http." + endpoint + ".extraHeader",
		Value: "Authorization: " + value,
	}
}

func gitEnvironment(entries ...entry) []string {
	environment := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_KEY_0=filter.lfs.smudge",
		"GIT_CONFIG_VALUE_0=git-lfs smudge --skip -- %f",
		"GIT_CONFIG_KEY_1=credential.helper",
		"GIT_CONFIG_VALUE_1=",
		"GIT_CONFIG_KEY_2=core.hooksPath",
		"GIT_CONFIG_VALUE_2=/dev/null",
		"GIT_CONFIG_KEY_3=http.postBuffer",
		fmt.Sprintf("GIT_CONFIG_VALUE_3=%d", gitHTTPPostBuffer),
	}

	for index, item := range entries {
		position := gitConfigBaseKeys + index

		environment = append(
			environment,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", position, item.Key),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", position, item.Value),
		)
	}

	return append(environment, fmt.Sprintf("GIT_CONFIG_COUNT=%d", gitConfigBaseKeys+len(entries)))
}

type gitTool struct {
	Redact *strings.Replacer
}

func (tool gitTool) run(
	ctx context.Context,
	dir string,
	environment []string,
	args ...string,
) (string, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}

	command := exec.CommandContext(ctx, "git", args...) //nolint:gosec

	command.Env = append(os.Environ(), environment...)

	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, tool.redact(string(output)))
	}

	return string(output), nil
}

func (tool gitTool) redact(output string) string {
	if tool.Redact == nil {
		return output
	}

	return tool.Redact.Replace(output)
}

func basicCredential(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}

func redactReplacer(secrets ...string) *strings.Replacer {
	pairs := make([]string, 0, len(secrets)*secretPairLength)

	for _, secret := range secrets {
		if secret != "" {
			pairs = append(pairs, secret, "***")
		}
	}

	return strings.NewReplacer(pairs...)
}
