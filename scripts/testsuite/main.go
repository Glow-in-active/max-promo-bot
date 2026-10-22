//go:build tools

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

func main() {
	if err := runTests(context.Background(), "go", os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runTests(ctx context.Context, goCommand string, args []string, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(ctx, goCommand, append([]string{"test", "-json"}, args...)...)
	command.Stderr = stderr
	results, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("read test process output: %w", err)
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start tests: %w", err)
	}

	resultErr := checkResults(results, stdout)
	if resultErr != nil {
		cancel()
	}
	processErr := command.Wait()
	if processErr != nil {
		processErr = fmt.Errorf("test process: %w", processErr)
	}
	return errors.Join(resultErr, processErr)
}
