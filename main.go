// Copyright (c) 2025 Sudo-Ivan
// Licensed under the MIT License

package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// EmailSet is a thread-safe set for storing unique email addresses.
type EmailSet struct {
	mu     sync.Mutex
	emails map[string]struct{}
}

// NewEmailSet creates and returns a new EmailSet with a pre-allocated capacity.
func NewEmailSet() *EmailSet {
	return &EmailSet{
		emails: make(map[string]struct{}, 1000),
	}
}

// Add adds an email address to the EmailSet.
func (e *EmailSet) Add(email string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emails[email] = struct{}{}
}

// GetAll returns a slice containing all email addresses in the EmailSet.
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
		fmt.Printf("\r%s: %s", pw.prefix, line)
	}
	return len(p), nil
}

// extractEmailsFromOutput extracts emails from git command output.
func extractEmailsFromOutput(output string, emailSet *EmailSet) int {
	count := 0
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Regular expression for email addresses
	emailRegex := regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)

	for scanner.Scan() {
		line := scanner.Text()
		// Find all email addresses in the line
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
	// Convert to absolute path
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path: %v", err)
	}

	// Get current working directory
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get current directory: %v", err)
	}

	// Ensure the path is within the current working directory
	if !strings.HasPrefix(absPath, cwd) {
		return "", fmt.Errorf("output path must be within current directory")
	}

	return absPath, nil
}

// writeOutput writes the emails to a file in the specified format.
func writeOutput(emails []string, format string, outputFile string) error {
	// Sanitize the output path
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

	// Write header
	if err := writer.Write([]string{"email"}); err != nil {
		return err
	}

	// Write emails
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
func worker(jobs <-chan job, emailSet *EmailSet, wg *sync.WaitGroup, workerID int) {
	defer wg.Done()
	for j := range jobs {
		fmt.Printf("\nWorker %d: Processing %s\n", workerID, j.repoURL)
		extractEmails(j.repoURL, j.token, emailSet, j.contributors, workerID)
	}
}

// extractEmails clones a repository, extracts email addresses from the git log, and adds them to the EmailSet.
func extractEmails(repoURL string, token string, emailSet *EmailSet, includeContributors bool, workerID int) {
	startTime := time.Now()
	cloneDir := fmt.Sprintf("/tmp/git-email-extractor-%d-%d", os.Getpid(), workerID)
	defer os.RemoveAll(cloneDir)

	// Clone repository with progress
	fmt.Printf("Worker %d: Cloning %s...\n", workerID, repoURL)
	cloneCmd := exec.Command("git", "clone", "--no-single-branch", "--progress", repoURL, cloneDir)
	if token != "" {
		cloneCmd.Env = append(os.Environ(), fmt.Sprintf("GIT_ASKPASS=echo %s", token))
	}

	// Create pipes for stdout and stderr
	stdout, err := cloneCmd.StdoutPipe()
	if err != nil {
		fmt.Printf("Worker %d: Error creating stdout pipe: %v\n", workerID, err)
		return
	}
	stderr, err := cloneCmd.StderrPipe()
	if err != nil {
		fmt.Printf("Worker %d: Error creating stderr pipe: %v\n", workerID, err)
		return
	}

	// Start the command
	if err := cloneCmd.Start(); err != nil {
		fmt.Printf("Worker %d: Error starting clone: %v\n", workerID, err)
		return
	}

	// Process stdout and stderr in separate goroutines
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
			fmt.Printf("\rWorker %d: %s", workerID, line)
		}
	}()

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.Contains(line, "Cloning into") {
				fmt.Printf("\rWorker %d: Error: %s", workerID, line)
			}
		}
	}()

	// Wait for the command to complete
	if err := cloneCmd.Wait(); err != nil {
		fmt.Printf("\nWorker %d: Error cloning %s: %v\n", workerID, repoURL, err)
		return
	}

	wg.Wait()
	fmt.Printf("\nWorker %d: Clone completed\n", workerID)

	// Extract commit author emails
	fmt.Printf("Worker %d: Extracting author emails...\n", workerID)
	cmd := exec.Command("git", "--no-pager", "log", "--all", "--pretty=format:%ae%n%an%n%b")
	cmd.Dir = cloneDir
	output, err := cmd.Output()
	if err != nil {
		fmt.Printf("Worker %d: Error getting author emails from %s: %v\n", workerID, repoURL, err)
		return
	}

	authorCount := extractEmailsFromOutput(string(output), emailSet)
	fmt.Printf("Worker %d: Found %d author emails\n", workerID, authorCount)

	if includeContributors {
		// Extract committer emails
		fmt.Printf("Worker %d: Extracting committer emails...\n", workerID)
		cmd = exec.Command("git", "--no-pager", "log", "--all", "--pretty=format:%ce%n%cn%n%b")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err != nil {
			fmt.Printf("Worker %d: Error getting committer emails from %s: %v\n", workerID, repoURL, err)
		} else {
			committerCount := extractEmailsFromOutput(string(output), emailSet)
			fmt.Printf("Worker %d: Found %d committer emails\n", workerID, committerCount)
		}

		// Extract emails from commit messages
		fmt.Printf("Worker %d: Extracting emails from commit messages...\n", workerID)
		cmd = exec.Command("git", "--no-pager", "log", "--all", "--pretty=format:%b")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err != nil {
			fmt.Printf("Worker %d: Error getting commit message emails from %s: %v\n", workerID, repoURL, err)
		} else {
			messageCount := extractEmailsFromOutput(string(output), emailSet)
			fmt.Printf("Worker %d: Found %d emails in commit messages\n", workerID, messageCount)
		}

		// Extract emails from git config
		fmt.Printf("Worker %d: Extracting emails from git config...\n", workerID)
		cmd = exec.Command("git", "config", "--get-regexp", "user\\.email")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err == nil {
			configCount := extractEmailsFromOutput(string(output), emailSet)
			fmt.Printf("Worker %d: Found %d emails in git config\n", workerID, configCount)
		}

		// Extract emails from all branches
		fmt.Printf("Worker %d: Extracting emails from all branches...\n", workerID)
		cmd = exec.Command("git", "branch", "-a")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err == nil {
			scanner := bufio.NewScanner(strings.NewReader(string(output)))
			for scanner.Scan() {
				branch := strings.TrimSpace(scanner.Text())
				if branch != "" {
					// Get emails from this branch
					cmd = exec.Command("git", "--no-pager", "log", branch, "--pretty=format:%ae%n%ce%n%b")
					cmd.Dir = cloneDir
					branchOutput, err := cmd.Output()
					if err == nil {
						extractEmailsFromOutput(string(branchOutput), emailSet)
					}
				}
			}
		}

		// Extract emails from tags
		fmt.Printf("Worker %d: Extracting emails from tags...\n", workerID)
		cmd = exec.Command("git", "tag", "-l")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err == nil {
			scanner := bufio.NewScanner(strings.NewReader(string(output)))
			for scanner.Scan() {
				tag := strings.TrimSpace(scanner.Text())
				if tag != "" {
					// Get emails from this tag
					cmd = exec.Command("git", "--no-pager", "log", tag, "--pretty=format:%ae%n%ce%n%b")
					cmd.Dir = cloneDir
					tagOutput, err := cmd.Output()
					if err == nil {
						extractEmailsFromOutput(string(tagOutput), emailSet)
					}
				}
			}
		}

		// Extract emails from reflog
		fmt.Printf("Worker %d: Extracting emails from reflog...\n", workerID)
		cmd = exec.Command("git", "--no-pager", "reflog", "--pretty=format:%ae%n%ce%n%b")
		cmd.Dir = cloneDir
		output, err = cmd.Output()
		if err == nil {
			reflogCount := extractEmailsFromOutput(string(output), emailSet)
			fmt.Printf("Worker %d: Found %d emails in reflog\n", workerID, reflogCount)
		}
	}

	duration := time.Since(startTime)
	fmt.Printf("Worker %d: Completed processing %s in %v\n", workerID, repoURL, duration)
}

// main is the entry point of the program.
// It parses command-line flags, starts worker goroutines,
// sends jobs to the workers, and prints the extracted email addresses.
func main() {
	token := flag.String("t", "", "GitHub/GitLab token for private repositories")
	workers := flag.Int("w", 4, "Number of worker goroutines")
	contributors := flag.Bool("c", false, "Include contributor emails (committers and signed-off-by)")
	format := flag.String("f", "txt", "Output format (json, csv, txt)")
	output := flag.String("o", "", "Output file (default: stdout)")
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Println("Usage: git-email-extractor [-t token] [-w workers] [-c] [-f format] [-o output] <repo-url1> [repo-url2 ...]")
		os.Exit(1)
	}

	emailSet := NewEmailSet()
	var wg sync.WaitGroup

	jobs := make(chan job, flag.NArg())

	fmt.Printf("Starting email extraction with %d workers...\n", *workers)
	wg.Add(*workers)
	for i := 0; i < *workers; i++ {
		go worker(jobs, emailSet, &wg, i+1)
	}

	for _, repoURL := range flag.Args() {
		jobs <- job{repoURL: repoURL, token: *token, contributors: *contributors}
	}
	close(jobs)

	wg.Wait()

	emails := emailSet.GetAll()
	fmt.Printf("\nExtraction complete! Found %d unique email addresses\n", len(emails))

	if *output != "" {
		// Ensure the output directory exists
		dir := filepath.Dir(*output)
		if dir != "." {
			if err := os.MkdirAll(dir, 0750); err != nil {
				fmt.Printf("Error creating output directory: %v\n", err)
				os.Exit(1)
			}
		}

		if err := writeOutput(emails, *format, *output); err != nil {
			fmt.Printf("Error writing output file: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Results written to %s\n", *output)
	} else {
		// Print to stdout
		for _, email := range emails {
			fmt.Println(email)
		}
	}
}
