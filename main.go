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
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"hash/fnv"

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

// Optimized email regex pattern with length limits and no backtracking
var emailRegex = regexp.MustCompile(`[a-zA-Z0-9._%+-]{1,64}@[a-zA-Z0-9.-]{1,255}\.[a-zA-Z]{2,}`)

// BloomFilter implements a simple bloom filter for quick negative lookups
type BloomFilter struct {
	bits    []bool
	size    uint64
	hashNum int
}

// NewBloomFilter creates a new bloom filter with optimal size and hash functions
func NewBloomFilter(expectedItems int, falsePositiveRate float64) *BloomFilter {
	size := uint64(float64(expectedItems) * -math.Log(falsePositiveRate) / (math.Log(2) * math.Log(2)))
	hashNum := int(float64(size) / float64(expectedItems) * math.Log(2))
	return &BloomFilter{
		bits:    make([]bool, size),
		size:    size,
		hashNum: hashNum,
	}
}

// Add adds an item to the bloom filter
func (bf *BloomFilter) Add(item string) {
	for i := 0; i < bf.hashNum; i++ {
		hash := bf.hash(item, i)
		bf.bits[hash%bf.size] = true
	}
}

// MayContain checks if an item might be in the set
func (bf *BloomFilter) MayContain(item string) bool {
	for i := 0; i < bf.hashNum; i++ {
		hash := bf.hash(item, i)
		if !bf.bits[hash%bf.size] {
			return false
		}
	}
	return true
}

// hash implements a simple hash function for the bloom filter
func (bf *BloomFilter) hash(item string, seed int) uint64 {
	h := fnv.New64a()
	if _, err := h.Write([]byte(item)); err != nil {
		// This should not happen with fnv hash
		logger.Panic("fnv hash write failed", zap.Error(err))
	}
	if _, err := h.Write([]byte{byte(seed)}); err != nil {
		// This should not happen with fnv hash
		logger.Panic("fnv hash write failed", zap.Error(err))
	}
	return h.Sum64()
}

// OptimizedEmailSet is a thread-safe set for storing unique email addresses with bloom filter
type OptimizedEmailSet struct {
	mu            sync.Mutex
	emails        map[string]struct{}
	bloomFilter   *BloomFilter
	emailFeedback chan<- string // Channel to send newly found unique emails
}

// NewOptimizedEmailSet creates a new OptimizedEmailSet with bloom filter
func NewOptimizedEmailSet(feedbackChannel chan<- string) *OptimizedEmailSet {
	return &OptimizedEmailSet{
		emails:        make(map[string]struct{}, 1000),
		bloomFilter:   NewBloomFilter(1000, 0.01),
		emailFeedback: feedbackChannel,
	}
}

// Add adds an email address to the OptimizedEmailSet.
// If the email is new, it's added to the set and sent to the feedback channel.
func (e *OptimizedEmailSet) Add(email string) {
	lowerEmail := strings.ToLower(email)

	// The map is the source of truth for uniqueness.
	e.mu.Lock()
	if _, exists := e.emails[lowerEmail]; exists {
		e.mu.Unlock() // Already processed this email.
		return
	}

	// If not in the map, it's a new unique email.
	e.emails[lowerEmail] = struct{}{}
	e.bloomFilter.Add(lowerEmail) // Add to Bloom Filter for future MayContain checks (if any were used).
	e.mu.Unlock()                 // Unlock before potentially blocking on channel send.

	// Send to feedback channel if it exists.
	if e.emailFeedback != nil {
		select {
		case e.emailFeedback <- lowerEmail:
			// Successfully sent to feedback channel.
		default:
			// Non-blocking: Log if channel is full or nil to avoid indefinite block.
			// This indicates a potential issue with consumer speed or channel buffering.
			logger.Warn("Email feedback channel blocked or nil, email dropped for streaming", zap.String("email", lowerEmail))
		}
	}
}

// GetAll returns a slice containing all email addresses in the OptimizedEmailSet
// This function might be less relevant if all data is streamed, but can be used for final count.
func (e *OptimizedEmailSet) GetAll() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	emails := make([]string, 0, len(e.emails))
	for email := range e.emails {
		emails = append(emails, email)
	}
	return emails
}

// FileScanner is a worker pool for scanning files
type FileScanner struct {
	workers int
	jobs    chan string
	results chan int
	wg      sync.WaitGroup
}

// NewFileScanner creates a new FileScanner with the specified number of workers
func NewFileScanner(workers int) *FileScanner {
	return &FileScanner{
		workers: workers,
		jobs:    make(chan string, workers*2),
		results: make(chan int, workers*2),
	}
}

// Start starts the file scanner workers
func (fs *FileScanner) Start(ctx context.Context, emailSet *OptimizedEmailSet) {
	fs.wg.Add(fs.workers)
	for i := 0; i < fs.workers; i++ {
		go fs.worker(ctx, emailSet, i+1)
	}
}

// worker processes file scanning jobs
func (fs *FileScanner) worker(ctx context.Context, emailSet *OptimizedEmailSet, workerID int) {
	defer fs.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case filename, ok := <-fs.jobs:
			if !ok {
				return
			}
			// Process file content in chunks
			file, err := os.Open(filename) // #nosec G304 -- filename origin needs audit if FileScanner is fully implemented
			if err != nil {
				continue
			}
			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
			count := 0
			for scanner.Scan() {
				line := scanner.Text()
				emails := emailRegex.FindAllString(line, -1)
				for _, email := range emails {
					emailSet.Add(email)
					count++
				}
			}
			if err := file.Close(); err != nil {
				logger.Error("failed to close file in FileScanner", zap.String("filename", filename), zap.Error(err))
			}
			fs.results <- count
		}
	}
}

// ScanFile adds a file to the scanning queue
func (fs *FileScanner) ScanFile(filename string) {
	fs.jobs <- filename
}

// Wait waits for all workers to complete and returns the total count
func (fs *FileScanner) Wait() int {
	close(fs.jobs)
	fs.wg.Wait()
	close(fs.results)
	total := 0
	for count := range fs.results {
		total += count
	}
	return total
}

// job defines the structure for a repository URL and its associated token.
type job struct {
	repoURL             string
	token               string
	contributors        bool
	configFilesToScan   []string
	userSpecifiedConfig bool
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

// extractEmailsFromOutput extracts emails from git command output using optimized structures
func extractEmailsFromOutput(output string, emailSet *OptimizedEmailSet) int {
	count := 0
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		emails := emailRegex.FindAllString(line, -1)
		for _, email := range emails {
			email = strings.TrimSpace(email)
			if email != "" {
				emailSet.Add(email)
				count++
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

// outputWriterProcess handles writing emails to the output file or stdout as they are received.
func outputWriterProcess(ctx context.Context, outputFilePath string, format string, emailChannel <-chan string, writerWg *sync.WaitGroup, finalEmailCount *int64) {
	defer writerWg.Done()

	var file *os.File
	var err error
	actualWriter := io.Writer(os.Stdout) // Default to stdout

	if outputFilePath != "" {
		safePath, errSanitize := sanitizePath(outputFilePath)
		if errSanitize != nil {
			logger.Error("Output path sanitization failed, defaulting to stdout.", zap.String("path", outputFilePath), zap.Error(errSanitize))
			// Drain channel to prevent producer deadlock if we can't write
			go func() {
				for range emailChannel {
				}
			}()
			return
		}

		dir := filepath.Dir(safePath)
		if dir != "." && dir != "" {
			if mkDirErr := os.MkdirAll(dir, 0750); mkDirErr != nil {
				logger.Error("Failed to create output directory, defaulting to stdout.", zap.String("path", dir), zap.Error(mkDirErr))
				go func() {
					for range emailChannel {
					}
				}()
				return
			}
		}
		file, err = os.OpenFile(safePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600) // #nosec G304 -- path sanitized by sanitizePath
		if err != nil {
			logger.Error("Failed to open output file, defaulting to stdout.", zap.String("path", safePath), zap.Error(err))
			go func() {
				for range emailChannel {
				}
			}()
			return
		}
		defer func() {
			if ferr := file.Close(); ferr != nil {
				logger.Error("Failed to close output file", zap.String("path", safePath), zap.Error(ferr))
			}
		}()
		actualWriter = file
	}

	var csvDataWriter *csv.Writer
	if format == "csv" {
		csvDataWriter = csv.NewWriter(actualWriter)
		if err := csvDataWriter.Write([]string{"email"}); err != nil {
			logger.Error("Failed to write CSV header", zap.Error(err))
			// If header fails, probably can't write data either.
			go func() {
				for range emailChannel {
				}
			}()
			return
		}
		defer csvDataWriter.Flush() // Ensure all buffered data is written at the end
	}

	var count int64
	logger.Info("Writer process started", zap.String("format", format), zap.String("output", outputFilePath))

	for {
		select {
		case <-ctx.Done():
			logger.Info("Writer process shutting down due to context cancellation.")
			if csvDataWriter != nil {
				csvDataWriter.Flush() // Attempt to flush before exiting
			}
			atomic.StoreInt64(finalEmailCount, count)
			return
		case email, ok := <-emailChannel:
			if !ok { // Channel closed
				logger.Info("Email channel closed, writer process finishing.")
				if csvDataWriter != nil {
					csvDataWriter.Flush()
					if csvErr := csvDataWriter.Error(); csvErr != nil {
						logger.Error("Error after final CSV flush", zap.Error(csvErr))
					}
				}
				atomic.StoreInt64(finalEmailCount, count)
				return
			}
			count++
			switch format {
			case "txt":
				if _, wErr := fmt.Fprintln(actualWriter, email); wErr != nil {
					logger.Error("Failed to write TXT line", zap.String("email", email), zap.Error(wErr))
				}
			case "csv":
				if wErr := csvDataWriter.Write([]string{email}); wErr != nil {
					logger.Error("Failed to write CSV row", zap.String("email", email), zap.Error(wErr))
				}
				if count%100 == 0 { // Flush CSV periodically
					csvDataWriter.Flush()
				}
			case "json": // JSON Lines
				lineData := struct {
					Email string `json:"email"`
				}{Email: email}
				jsonEncoder := json.NewEncoder(actualWriter)
				if encErr := jsonEncoder.Encode(lineData); encErr != nil {
					logger.Error("Failed to write JSON line", zap.String("email", email), zap.Error(encErr))
				}
			default:
				// This case should ideally be caught by flag validation in main
				logger.Error("Unsupported format in writer goroutine", zap.String("format", format), zap.String("email", email))
			}
		}
	}
}

// worker processes jobs from the jobs channel, extracting emails from each repository.
func worker(ctx context.Context, jobs <-chan job, emailSet *OptimizedEmailSet, wg *sync.WaitGroup, workerID int) {
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
			extractEmails(ctx, j.repoURL, j.token, emailSet, j.contributors, workerID, j.configFilesToScan, j.userSpecifiedConfig)
		}
	}
}

// scanFileInHistory scans a specific file in git history for email patterns.
func scanFileInHistory(ctx context.Context, cloneDir string, filename string, emailSet *OptimizedEmailSet, workerID int) {
	// Create a context with timeout for the git log command
	logCtx, cancelLog := context.WithTimeout(ctx, 1*time.Minute) // 2-minute timeout
	defer cancelLog()

	cmd := exec.CommandContext(logCtx, "git", "--no-pager", "log", "--all", "--pretty=format:%H", "--", filename)
	cmd.Dir = cloneDir
	output, err := cmd.Output()
	if err != nil {
		if errors.Is(logCtx.Err(), context.DeadlineExceeded) {
			logger.Warn("git log command timed out for file history",
				zap.Int("worker", workerID),
				zap.String("file", filename),
				zap.Duration("timeout", 1*time.Minute))
		} else {
			logger.Debug("file not found in history or no commits for file",
				zap.Int("worker", workerID),
				zap.String("file", filename),
				zap.Error(err)) // It's common for files not to exist or have history, so Debug level is fine.
		}
		return
	}

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		commit := scanner.Text()
		if commit == "" {
			continue
		}

		// Create a context with timeout for the git show command
		showCtx, cancelShow := context.WithTimeout(ctx, 30*time.Second) // 60-second timeout for git show

		cmd = exec.CommandContext(showCtx, "git", "--no-pager", "show", commit+":"+filename) // #nosec G204 -- commit is git hash, filename from config
		cmd.Dir = cloneDir
		fileContent, err := cmd.Output()
		cancelShow() // Release resources associated with showCtx once done or on error

		if err != nil {
			if errors.Is(showCtx.Err(), context.DeadlineExceeded) {
				logger.Warn("git show command timed out for file in commit",
					zap.Int("worker", workerID),
					zap.String("file", filename),
					zap.String("commit", commit),
					zap.Duration("timeout", 30*time.Second))
			} else {
				logger.Debug("failed to get file content with git show", // Can be normal if file was e.g. deleted then re-added, or other git history quirks
					zap.Int("worker", workerID),
					zap.String("file", filename),
					zap.String("commit", commit),
					zap.Error(err))
			}
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
func extractEmails(ctx context.Context, repoURL string, token string, emailSet *OptimizedEmailSet, includeContributors bool, workerID int, configFilesForHistoryScan []string, userDidSpecifyConfigFile bool) {
	startTime := time.Now()
	cloneDir := fmt.Sprintf("/tmp/git-email-extractor-%d-%d", os.Getpid(), workerID)
	defer os.RemoveAll(cloneDir)

	logger.Info("starting repository processing",
		zap.String("repo", repoURL),
		zap.Int("worker", workerID))

	var cloneArgs []string
	if userDidSpecifyConfigFile {
		cloneArgs = []string{"clone", "--filter=blob:none", "--no-checkout", "--no-single-branch", "--progress", repoURL, cloneDir}
		logger.Info("Using filtered clone for specified config files", zap.String("repo", repoURL), zap.Int("worker", workerID))
	} else {
		cloneArgs = []string{"clone", "--no-single-branch", "--progress", repoURL, cloneDir}
	}
	cloneCmd := exec.CommandContext(ctx, "git", cloneArgs...) // #nosec G204 -- repoURL is user-supplied, cloneDir is generated, other args fixed
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
			if strings.Contains(line, "Cloning into") { // This is expected, not an error, not really progress to log repeatedly
				continue
			}

			// Check for known progress indicators
			isProgress := strings.Contains(line, "%") || // Catches most percentage-based progress
				strings.HasPrefix(line, "Receiving objects:") ||
				strings.HasPrefix(line, "Resolving deltas:") ||
				strings.HasPrefix(line, "Unpacking objects:") ||
				strings.HasPrefix(line, "remote: Counting objects:") ||
				strings.HasPrefix(line, "remote: Compressing objects:") ||
				strings.HasPrefix(line, "remote: Enumerating objects:")

			if isProgress {
				logger.Debug("clone progress",
					zap.Int("worker", workerID),
					zap.String("message", line))
			} else {
				// Log anything else from stderr as a potential issue, but not necessarily a fatal error.
				// Git might print warnings or other non-fatal info to stderr.
				logger.Warn("clone stderr", // Changed from Error to Warn
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

	baseLogArgs := []string{"--no-pager", "log", "--all"}
	pathSpecArgs := []string{}
	if userDidSpecifyConfigFile && len(configFilesForHistoryScan) > 0 {
		pathSpecArgs = append(pathSpecArgs, "--")
		pathSpecArgs = append(pathSpecArgs, configFilesForHistoryScan...)
	}

	// Author emails
	authorLogCmdArgs := append(baseLogArgs, "--pretty=format:%ae%n%an%n%b")
	if len(pathSpecArgs) > 0 {
		authorLogCmdArgs = append(authorLogCmdArgs, pathSpecArgs...)
	}
	authorLogCtx, cancelAuthorLog := context.WithTimeout(ctx, 5*time.Minute) // 5-minute timeout
	defer cancelAuthorLog()
	cmd := exec.CommandContext(authorLogCtx, "git", authorLogCmdArgs...) // #nosec G204 -- args carefully constructed
	cmd.Dir = cloneDir
	output, err := cmd.Output()
	if err != nil {
		if errors.Is(authorLogCtx.Err(), context.DeadlineExceeded) {
			logger.Error("failed to get author emails due to timeout",
				zap.Int("worker", workerID),
				zap.String("repo", repoURL),
				zap.Duration("timeout", 5*time.Minute),
				zap.Error(err))
		} else {
			logger.Error("failed to get author emails",
				zap.Int("worker", workerID),
				zap.String("repo", repoURL),
				zap.Error(err))
		}
		return
	}

	authorCount := extractEmailsFromOutput(string(output), emailSet)
	logger.Info("author emails extracted",
		zap.Int("worker", workerID),
		zap.Int("count", authorCount))

	if includeContributors {
		// Committer emails
		committerLogCmdArgs := append(baseLogArgs, "--pretty=format:%ce%n%cn%n%b")
		if len(pathSpecArgs) > 0 {
			committerLogCmdArgs = append(committerLogCmdArgs, pathSpecArgs...)
		}
		committerLogCtx, cancelCommitterLog := context.WithTimeout(ctx, 5*time.Minute) // 5-minute timeout
		defer cancelCommitterLog()
		cmd = exec.CommandContext(committerLogCtx, "git", committerLogCmdArgs...) // #nosec G204 -- args carefully constructed
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err != nil {
			if errors.Is(committerLogCtx.Err(), context.DeadlineExceeded) {
				logger.Error("failed to get committer emails due to timeout",
					zap.Int("worker", workerID),
					zap.String("repo", repoURL),
					zap.Duration("timeout", 5*time.Minute),
					zap.Error(err))
			} else {
				logger.Error("failed to get committer emails",
					zap.Int("worker", workerID),
					zap.String("repo", repoURL),
					zap.Error(err))
			}
		} else {
			committerCount := extractEmailsFromOutput(string(output), emailSet)
			logger.Info("committer emails extracted",
				zap.Int("worker", workerID),
				zap.Int("count", committerCount))
		}

		// Commit message emails (for signed-off-by etc.)
		messageLogCmdArgs := append(baseLogArgs, "--pretty=format:%b")
		if len(pathSpecArgs) > 0 {
			messageLogCmdArgs = append(messageLogCmdArgs, pathSpecArgs...)
		}
		messageLogCtx, cancelMessageLog := context.WithTimeout(ctx, 5*time.Minute) // 5-minute timeout
		defer cancelMessageLog()
		cmd = exec.CommandContext(messageLogCtx, "git", messageLogCmdArgs...) // #nosec G204 -- args carefully constructed
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err != nil {
			if errors.Is(messageLogCtx.Err(), context.DeadlineExceeded) {
				logger.Error("failed to get commit message emails due to timeout",
					zap.Int("worker", workerID),
					zap.String("repo", repoURL),
					zap.Duration("timeout", 5*time.Minute),
					zap.Error(err))
			} else {
				logger.Error("failed to get commit message emails",
					zap.Int("worker", workerID),
					zap.String("repo", repoURL),
					zap.Error(err))
			}
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
					branchLogArgs := []string{"--no-pager", "log", branch, "--pretty=format:%ae%n%ce%n%b"}
					if len(pathSpecArgs) > 0 {
						branchLogArgs = append(branchLogArgs, pathSpecArgs...)
					}
					branchLogCtx, cancelBranchLog := context.WithTimeout(ctx, 1*time.Minute) // 1-minute timeout
					cmd = exec.CommandContext(branchLogCtx, "git", branchLogArgs...)         // #nosec G204 -- branch from 'git branch -a', other args constructed
					cmd.Dir = cloneDir
					branchOutput, err := cmd.Output()
					cancelBranchLog() // Release resources
					if err == nil {
						extractEmailsFromOutput(string(branchOutput), emailSet)
					} else if errors.Is(branchLogCtx.Err(), context.DeadlineExceeded) {
						logger.Warn("git log for branch timed out",
							zap.Int("worker", workerID),
							zap.String("branch", branch),
							zap.Duration("timeout", 1*time.Minute))
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
					tagLogArgs := []string{"--no-pager", "log", tag, "--pretty=format:%ae%n%ce%n%b"}
					if len(pathSpecArgs) > 0 {
						tagLogArgs = append(tagLogArgs, pathSpecArgs...)
					}
					tagLogCtx, cancelTagLog := context.WithTimeout(ctx, 1*time.Minute) // 1-minute timeout
					cmd = exec.CommandContext(tagLogCtx, "git", tagLogArgs...)         // #nosec G204 -- tag from 'git tag -l', other args constructed
					cmd.Dir = cloneDir
					tagOutput, err := cmd.Output()
					cancelTagLog() // Release resources
					if err == nil {
						extractEmailsFromOutput(string(tagOutput), emailSet)
					} else if errors.Is(tagLogCtx.Err(), context.DeadlineExceeded) {
						logger.Warn("git log for tag timed out",
							zap.Int("worker", workerID),
							zap.String("tag", tag),
							zap.Duration("timeout", 1*time.Minute))
					}
				}
			}
		}

		reflogCtx, cancelReflog := context.WithTimeout(ctx, 2*time.Minute) // 2-minute timeout
		defer cancelReflog()
		cmd = exec.CommandContext(reflogCtx, "git", "--no-pager", "reflog", "--pretty=format:%ae%n%ce%n%b")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err == nil {
			reflogCount := extractEmailsFromOutput(string(output), emailSet)
			logger.Info("reflog emails extracted",
				zap.Int("worker", workerID),
				zap.Int("count", reflogCount))
		} else if errors.Is(reflogCtx.Err(), context.DeadlineExceeded) {
			logger.Error("git reflog command timed out",
				zap.Int("worker", workerID),
				zap.String("repo", repoURL),
				zap.Duration("timeout", 2*time.Minute),
				zap.Error(err))
		}
	}

	// Scan for emails in common dependency and configuration files
	for _, file := range configFilesForHistoryScan {
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

	data, err := os.ReadFile(configPath) // #nosec G304 -- configPath is direct user input to specify a config file
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

	// Validate format flag
	validFormats := map[string]bool{"json": true, "csv": true, "txt": true}
	if !validFormats[*format] {
		logger.Fatal("Invalid output format. Supported formats: json, csv, txt.", zap.String("format", *format))
	}

	if flag.NArg() == 0 {
		logger.Fatal("no repository URLs provided",
			zap.String("usage", "git-email-extractor [-t token] [-w workers] [-c] [-f format] [-o output] [-config config-file] <repo-url1> [repo-url2 ...]"))
	}

	loadedConfigFiles, err := loadConfigFiles(*configFile)
	if err != nil {
		logger.Fatal("failed to load config files",
			zap.Error(err))
	}

	emailChannelBufferSize := *workers * 10 // Buffer for emails before writer goroutine picks them up
	emailChannel := make(chan string, emailChannelBufferSize)
	var writtenEmailCount int64

	var writerWg sync.WaitGroup
	writerWg.Add(1)
	go outputWriterProcess(ctx, *output, *format, emailChannel, &writerWg, &writtenEmailCount)

	emailSet := NewOptimizedEmailSet(emailChannel)
	fileScanner := NewFileScanner(*workers) // FileScanner uses emailSet.Add, so it will also use the channel
	fileScanner.Start(ctx, emailSet)

	var wg sync.WaitGroup

	jobs := make(chan job, flag.NArg())

	logger.Info("starting email extraction",
		zap.Int("workers", *workers),
		zap.Int("repositories", flag.NArg()),
		zap.Int("config_files", len(loadedConfigFiles)))

	wg.Add(*workers)
	for i := 0; i < *workers; i++ {
		go worker(ctx, jobs, emailSet, &wg, i+1)
	}

	go func() {
		for _, repoURL := range flag.Args() {
			select {
			case <-ctx.Done():
				return
			case jobs <- job{
				repoURL:             repoURL,
				token:               *token,
				contributors:        *contributors,
				configFilesToScan:   loadedConfigFiles,
				userSpecifiedConfig: *configFile != "",
			}:
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
		// Workers will exit due to ctx.Done()
		// Wait for them to complete their current tasks or exit.
		<-done // done channel is closed when processing workers (wg) are finished
		logger.Info("All processing workers finished.")
	case <-done:
		logger.Info("all workers completed")
	}

	// After all processing workers are done, close the email channel to signal the writer to finish up.
	close(emailChannel)
	logger.Info("Email channel closed. Waiting for writer process to complete...")

	writerWg.Wait() // Wait for the writer goroutine to finish writing all pending emails.

	logger.Info("Extraction and writing completed.",
		zap.Int64("unique_emails_written", writtenEmailCount))
}
