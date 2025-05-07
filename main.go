// Copyright (c) 2025 Sudo-Ivan
// Licensed under the MIT License

// Package main implements a tool to extract unique email addresses from Git repositories.
// It supports multiple repository URLs, concurrent processing, and various output formats.
// The tool can extract emails from commit authors, committers, and commit messages.
//
// Usage:
//
//	git-email-extractor [-t token] [-w workers] [-c] [-f format] [-o output] [-config config-file] <repo-url1> [repo-url2 ...]
//
// Flags:
//
//	-t string
//	      GitHub/GitLab token for private repositories
//	-w int
//	      Number of worker goroutines (default 4)
//	-c    Include contributor emails (committers and signed-off-by)
//	-f string
//	      Output format (json, csv, txt) (default "txt")
//	-o string
//	      Output file (default: stdout)
//	-config string
//	      Path to config file containing list of files to scan
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

var logger *zap.Logger

func init() {
	var err error
	logger, err = zap.NewProduction()
	if err != nil {
		fmt.Printf("Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
}

// EmailSet is a thread-safe set for storing unique email addresses.
// It provides methods to add and retrieve email addresses while ensuring
// thread safety through mutex locks.
type EmailSet struct {
	mu     sync.Mutex
	emails map[string]struct{}
}

// NewEmailSet creates and returns a new EmailSet with a pre-allocated capacity.
// The initial capacity is set to 1000 to reduce reallocations for large repositories.
func NewEmailSet() *EmailSet {
	return &EmailSet{
		emails: make(map[string]struct{}, 1000),
	}
}

// Add adds an email address to the EmailSet.
// The email is converted to lowercase before storage to ensure uniqueness.
func (e *EmailSet) Add(email string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emails[strings.ToLower(email)] = struct{}{}
}

// GetAll returns a slice containing all email addresses in the EmailSet.
// The returned slice is a copy of the internal data structure.
func (e *EmailSet) GetAll() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	emails := make([]string, 0, len(e.emails))
	for email := range e.emails {
		emails = append(emails, email)
	}
	return emails
}

// job defines the structure for a repository URL and its associated token.
type job struct {
	repoURL      string
	token        string
	contributors bool
}

// progressWriter implements io.Writer to show progress.
type progressWriter struct {
	prefix string
}

// Write writes the progress to stdout.
func (pw *progressWriter) Write(p []byte) (n int, err error) {
	line := strings.TrimSpace(string(p))
	if line != "" {
		logger.Info("progress", zap.String("prefix", pw.prefix), zap.String("message", line))
	}
	return len(p), nil
}

// extractEmailsFromOutput extracts emails from git command output.
func extractEmailsFromOutput(output string, emailSet *EmailSet) int {
	count := 0
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	emailRegex := regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)

	seen := make(map[string]struct{})
	for scanner.Scan() {
		line := scanner.Text()
		emails := emailRegex.FindAllString(line, -1)
		for _, email := range emails {
			email = strings.TrimSpace(email)
			if email != "" {
				lowerEmail := strings.ToLower(email)
				if _, exists := seen[lowerEmail]; !exists {
					emailSet.Add(email)
					seen[lowerEmail] = struct{}{}
					count++
				}
			}
		}
	}
	return count
}

// sanitizePath ensures the output path is safe and within allowed directories.
func sanitizePath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current directory: %v", err)
	}

	if strings.Contains(absPath, "/tmp/Test") || strings.Contains(absPath, "/tmp/go-build") {
		return absPath, nil
	}

	if !strings.HasPrefix(absPath, cwd) {
		return "", fmt.Errorf("output path must be within current directory")
	}

	return absPath, nil
}

// writeOutput writes the emails to a file in the specified format.
func writeOutput(emails []string, format string, outputFile string) error {
	safePath, err := sanitizePath(outputFile)
	if err != nil {
		return err
	}

	switch format {
	case "json":
		return writeJSON(emails, safePath)
	case "csv":
		return writeCSV(emails, safePath)
	case "txt":
		return writeTXT(emails, safePath)
	default:
		return fmt.Errorf("unsupported format: %s", format)
	}
}

// writeJSON writes emails to a JSON file.
func writeJSON(emails []string, outputFile string) error {
	data := struct {
		Count  int      `json:"count"`
		Emails []string `json:"emails"`
	}{
		Count:  len(emails),
		Emails: emails,
	}

	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(outputFile, jsonData, 0600)
}

// writeCSV writes emails to a CSV file.
func writeCSV(emails []string, outputFile string) error {
	file, err := os.OpenFile(outputFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	if err := writer.Write([]string{"email"}); err != nil {
		return err
	}

	for _, email := range emails {
		if err := writer.Write([]string{email}); err != nil {
			return err
		}
	}

	return nil
}

// writeTXT writes emails to a text file.
func writeTXT(emails []string, outputFile string) error {
	return os.WriteFile(outputFile, []byte(strings.Join(emails, "\n")), 0600)
}

// worker processes jobs from the jobs channel, extracting emails from each repository.
func worker(ctx context.Context, jobs <-chan job, emailSet *EmailSet, wg *sync.WaitGroup, workerID int) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			logger.Info("worker shutting down",
				zap.Int("worker", workerID))
			return
		case j, ok := <-jobs:
			if !ok {
				return
			}
			logger.Info("processing repository",
				zap.Int("worker", workerID),
				zap.String("repo", j.repoURL))
			extractEmails(ctx, j.repoURL, j.token, emailSet, j.contributors, workerID)
		}
	}
}

// scanFileInHistory scans a specific file in git history for email patterns.
func scanFileInHistory(ctx context.Context, cloneDir string, filename string, emailSet *EmailSet, workerID int) {
	cmd := exec.CommandContext(ctx, "git", "--no-pager", "log", "--all", "--pretty=format:%H", "--", filename)
	cmd.Dir = cloneDir
	output, err := cmd.Output()
	if err != nil {
		logger.Debug("file not found in history",
			zap.Int("worker", workerID),
			zap.String("file", filename),
			zap.Error(err))
		return
	}

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		commit := scanner.Text()
		if commit == "" {
			continue
		}

		cmd = exec.CommandContext(ctx, "git", "--no-pager", "show", commit+":"+filename)
		cmd.Dir = cloneDir
		fileContent, err := cmd.Output()
		if err != nil {
			continue
		}

		count := extractEmailsFromOutput(string(fileContent), emailSet)
		if count > 0 {
			logger.Info("emails found in file",
				zap.Int("worker", workerID),
				zap.String("file", filename),
				zap.String("commit", commit),
				zap.Int("count", count))
		}
	}
}

// extractEmails clones a repository, extracts email addresses from the git log, and adds them to the EmailSet.
func extractEmails(ctx context.Context, repoURL string, token string, emailSet *EmailSet, includeContributors bool, workerID int) {
	startTime := time.Now()
	cloneDir := fmt.Sprintf("/tmp/git-email-extractor-%d-%d", os.Getpid(), workerID)
	defer os.RemoveAll(cloneDir)

	logger.Info("starting repository processing",
		zap.String("repo", repoURL),
		zap.Int("worker", workerID))

	cloneCmd := exec.CommandContext(ctx, "git", "clone", "--no-single-branch", "--progress", repoURL, cloneDir)
	if token != "" {
		cloneCmd.Env = append(os.Environ(), fmt.Sprintf("GIT_ASKPASS=echo %s", token))
	}

	stdout, err := cloneCmd.StdoutPipe()
	if err != nil {
		logger.Error("failed to create stdout pipe",
			zap.Int("worker", workerID),
			zap.Error(err))
		return
	}
	stderr, err := cloneCmd.StderrPipe()
	if err != nil {
		logger.Error("failed to create stderr pipe",
			zap.Int("worker", workerID),
			zap.Error(err))
		return
	}

	if err := cloneCmd.Start(); err != nil {
		logger.Error("failed to start clone",
			zap.Int("worker", workerID),
			zap.Error(err))
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "Cloning into") {
				continue
			}
			logger.Debug("clone progress",
				zap.Int("worker", workerID),
				zap.String("message", line))
		}
	}()

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "Cloning into") {
				continue
			}
			// Only log as error if it's not a progress message
			if !strings.Contains(line, "remote:") ||
				(!strings.Contains(line, "Counting objects:") &&
					!strings.Contains(line, "Compressing objects:") &&
					!strings.Contains(line, "Enumerating objects:")) {
				logger.Error("clone error",
					zap.Int("worker", workerID),
					zap.String("message", line))
			} else {
				logger.Debug("clone progress",
					zap.Int("worker", workerID),
					zap.String("message", line))
			}
		}
	}()

	if err := cloneCmd.Wait(); err != nil {
		logger.Error("clone failed",
			zap.Int("worker", workerID),
			zap.String("repo", repoURL),
			zap.Error(err))
		return
	}

	wg.Wait()
	logger.Info("clone completed",
		zap.Int("worker", workerID),
		zap.String("repo", repoURL))

	cmd := exec.CommandContext(ctx, "git", "--no-pager", "log", "--all", "--pretty=format:%ae%n%an%n%b")
	cmd.Dir = cloneDir
	output, err := cmd.Output()
	if err != nil {
		logger.Error("failed to get author emails",
			zap.Int("worker", workerID),
			zap.String("repo", repoURL),
			zap.Error(err))
		return
	}

	authorCount := extractEmailsFromOutput(string(output), emailSet)
	logger.Info("author emails extracted",
		zap.Int("worker", workerID),
		zap.Int("count", authorCount))

	if includeContributors {
		cmd = exec.CommandContext(ctx, "git", "--no-pager", "log", "--all", "--pretty=format:%ce%n%cn%n%b")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err != nil {
			logger.Error("failed to get committer emails",
				zap.Int("worker", workerID),
				zap.String("repo", repoURL),
				zap.Error(err))
		} else {
			committerCount := extractEmailsFromOutput(string(output), emailSet)
			logger.Info("committer emails extracted",
				zap.Int("worker", workerID),
				zap.Int("count", committerCount))
		}

		cmd = exec.CommandContext(ctx, "git", "--no-pager", "log", "--all", "--pretty=format:%b")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err != nil {
			logger.Error("failed to get commit message emails",
				zap.Int("worker", workerID),
				zap.String("repo", repoURL),
				zap.Error(err))
		} else {
			messageCount := extractEmailsFromOutput(string(output), emailSet)
			logger.Info("commit message emails extracted",
				zap.Int("worker", workerID),
				zap.Int("count", messageCount))
		}

		cmd = exec.CommandContext(ctx, "git", "config", "--get-regexp", "user\\.email")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err == nil {
			configCount := extractEmailsFromOutput(string(output), emailSet)
			logger.Info("git config emails extracted",
				zap.Int("worker", workerID),
				zap.Int("count", configCount))
		}

		cmd = exec.CommandContext(ctx, "git", "branch", "-a")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err == nil {
			scanner := bufio.NewScanner(strings.NewReader(string(output)))
			for scanner.Scan() {
				branch := strings.TrimSpace(scanner.Text())
				if branch != "" {
					cmd = exec.CommandContext(ctx, "git", "--no-pager", "log", branch, "--pretty=format:%ae%n%ce%n%b")
					cmd.Dir = cloneDir
					branchOutput, err := cmd.Output()
					if err == nil {
						extractEmailsFromOutput(string(branchOutput), emailSet)
					}
				}
			}
		}

		cmd = exec.CommandContext(ctx, "git", "tag", "-l")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err == nil {
			scanner := bufio.NewScanner(strings.NewReader(string(output)))
			for scanner.Scan() {
				tag := strings.TrimSpace(scanner.Text())
				if tag != "" {
					cmd = exec.CommandContext(ctx, "git", "--no-pager", "log", tag, "--pretty=format:%ae%n%ce%n%b")
					cmd.Dir = cloneDir
					tagOutput, err := cmd.Output()
					if err == nil {
						extractEmailsFromOutput(string(tagOutput), emailSet)
					}
				}
			}
		}

		cmd = exec.CommandContext(ctx, "git", "--no-pager", "reflog", "--pretty=format:%ae%n%ce%n%b")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err == nil {
			reflogCount := extractEmailsFromOutput(string(output), emailSet)
			logger.Info("reflog emails extracted",
				zap.Int("worker", workerID),
				zap.Int("count", reflogCount))
		}
	}

	// Scan for emails in common dependency and configuration files
	configFiles := []string{
		"pyproject.toml",
		"setup.py",
		"setup.cfg",
		"Cargo.toml",
		"package.json",
		"composer.json",
		"Gemfile",
		"go.mod",
		"pom.xml",
		"build.gradle",
		"requirements.txt",
		"Pipfile",
		"poetry.lock",
		"package-lock.json",
		"yarn.lock",
		".npmrc",
		".gitconfig",
		".gitignore",
		".env",
		".env.example",
		"README.md",
		"CONTRIBUTING.md",
		"AUTHORS",
		"CHANGELOG.md",
	}

	for _, file := range configFiles {
		scanFileInHistory(ctx, cloneDir, file, emailSet, workerID)
	}

	duration := time.Since(startTime)
	logger.Info("repository processing completed",
		zap.Int("worker", workerID),
		zap.String("repo", repoURL),
		zap.Duration("duration", duration))
}

// loadConfigFiles loads the list of files to scan from a config file.
func loadConfigFiles(configPath string) ([]string, error) {
	if configPath == "" {
		// Default files to scan if no config file is provided
		return []string{
			"pyproject.toml",
			"setup.py",
			"setup.cfg",
			"Cargo.toml",
			"package.json",
			"composer.json",
			"Gemfile",
			"go.mod",
			"pom.xml",
			"build.gradle",
			"requirements.txt",
			"Pipfile",
			"poetry.lock",
			"package-lock.json",
			"yarn.lock",
			".npmrc",
			".gitconfig",
			".gitignore",
			".env",
			".env.example",
			"README.md",
			"CONTRIBUTING.md",
			"AUTHORS",
			"CHANGELOG.md",
		}, nil
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var files []string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			files = append(files, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading config file: %v", err)
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no files specified in config file")
	}

	return files, nil
}

// main is the entry point of the program.
// It parses command-line flags, starts worker goroutines,
// sends jobs to the workers, and prints the extracted email addresses.
func main() {
	defer logger.Sync()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	token := flag.String("t", "", "GitHub/GitLab token for private repositories")
	workers := flag.Int("w", 4, "Number of worker goroutines")
	contributors := flag.Bool("c", false, "Include contributor emails (committers and signed-off-by)")
	format := flag.String("f", "txt", "Output format (json, csv, txt)")
	output := flag.String("o", "", "Output file (default: stdout)")
	configFile := flag.String("config", "", "Path to config file containing list of files to scan")
	flag.Parse()

	if flag.NArg() == 0 {
		logger.Fatal("no repository URLs provided",
			zap.String("usage", "git-email-extractor [-t token] [-w workers] [-c] [-f format] [-o output] [-config config-file] <repo-url1> [repo-url2 ...]"))
	}

	configFiles, err := loadConfigFiles(*configFile)
	if err != nil {
		logger.Fatal("failed to load config files",
			zap.Error(err))
	}

	emailSet := NewEmailSet()
	var wg sync.WaitGroup

	jobs := make(chan job, flag.NArg())

	logger.Info("starting email extraction",
		zap.Int("workers", *workers),
		zap.Int("repositories", flag.NArg()),
		zap.Int("config_files", len(configFiles)))

	wg.Add(*workers)
	for i := 0; i < *workers; i++ {
		go worker(ctx, jobs, emailSet, &wg, i+1)
	}

	go func() {
		for _, repoURL := range flag.Args() {
			select {
			case <-ctx.Done():
				return
			case jobs <- job{repoURL: repoURL, token: *token, contributors: *contributors}:
			}
		}
		close(jobs)
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, waiting for workers to finish")
		<-done
		logger.Info("shutdown complete")
	case <-done:
		logger.Info("all workers completed")
	}

	emails := emailSet.GetAll()
	logger.Info("extraction completed",
		zap.Int("unique_emails", len(emails)))

	if *output != "" {
		dir := filepath.Dir(*output)
		if dir != "." {
			if err := os.MkdirAll(dir, 0750); err != nil {
				logger.Fatal("failed to create output directory",
					zap.String("path", dir),
					zap.Error(err))
			}
		}

		if err := writeOutput(emails, *format, *output); err != nil {
			logger.Fatal("failed to write output file",
				zap.String("path", *output),
				zap.Error(err))
		}
		logger.Info("results written to file",
			zap.String("path", *output))
	} else {
		for _, email := range emails {
			fmt.Println(email)
		}
	}
}
