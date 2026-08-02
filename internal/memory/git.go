package memory

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (s *Store) prepareGitBaseline(ctx context.Context) error {
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	if err := atomicWrite(filepath.Join(s.root, ".gitignore"), []byte("state.json\n.skills-next/\n.skills-previous/\n")); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(s.root, ".git")); errors.Is(err, os.ErrNotExist) {
		if _, runErr := s.runGit(ctx, "init", "--quiet"); runErr != nil {
			return runErr
		}
		_, _ = s.runGit(ctx, "config", "user.name", "SuperCode Memory")
		_, _ = s.runGit(ctx, "config", "user.email", "memory@localhost")
		if _, runErr := s.runGit(ctx, "add", "-A"); runErr != nil {
			return runErr
		}
		_, _ = s.runGit(ctx, "commit", "--quiet", "--allow-empty", "-m", "Initialize memory baseline")
	}
	return nil
}

func (s *Store) memoryGitDiff(ctx context.Context) string {
	if err := s.prepareGitBaseline(ctx); err != nil {
		return "Git baseline unavailable: " + err.Error()
	}
	_, _ = s.runGit(ctx, "add", "-N", ".")
	output, err := s.runGit(ctx, "diff", "--no-ext-diff", "--", ".")
	if err != nil {
		return "Git diff unavailable: " + err.Error()
	}
	if strings.TrimSpace(output) == "" {
		return "(no Git workspace changes)"
	}
	if len(output) > 200_000 {
		output = output[:200_000] + "\n[Git diff truncated]"
	}
	return output
}

func (s *Store) commitGitBaseline(ctx context.Context) error {
	if err := s.prepareGitBaseline(ctx); err != nil {
		return err
	}
	if _, err := s.runGit(ctx, "add", "-A"); err != nil {
		return err
	}
	status, err := s.runGit(ctx, "status", "--porcelain")
	if err != nil || strings.TrimSpace(status) == "" {
		return err
	}
	_, err = s.runGit(ctx, "commit", "--quiet", "-m", "Update consolidated memory")
	return err
}

func (s *Store) runGit(ctx context.Context, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", s.root}, arguments...)...)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}
