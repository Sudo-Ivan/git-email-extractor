package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestEmailSet(t *testing.T) {
	tests := []struct {
		name     string
		emails   []string
		expected int
	}{
		{
			name:     "empty set",
			emails:   []string{},
			expected: 0,
		},
		{
			name:     "single email",
			emails:   []string{"test@example.com"},
			expected: 1,
		},
		{
			name:     "duplicate emails",
			emails:   []string{"test@example.com", "test@example.com"},
			expected: 1,
		},
		{
			name:     "multiple unique emails",
			emails:   []string{"test1@example.com", "test2@example.com", "test3@example.com"},
			expected: 3,
		},
		{
			name:     "mixed case emails",
			emails:   []string{"Test@Example.com", "test@example.com"},
			expected: 1,
		},
		{
			name:     "special characters in emails",
			emails:   []string{"test+label@example.com", "test.label@example.com"},
			expected: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emailSet := NewEmailSet()
			for _, email := range tt.emails {
				emailSet.Add(email)
			}
			emails := emailSet.GetAll()
			assert.Equal(t, tt.expected, len(emails))

			seen := make(map[string]struct{})
			for _, email := range emails {
				_, exists := seen[strings.ToLower(email)]
				assert.False(t, exists, "duplicate email found: %s", email)
				seen[strings.ToLower(email)] = struct{}{}
			}
		})
	}
}

func TestExtractEmailsFromOutput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "empty input",
			input:    "",
			expected: []string{},
		},
		{
			name:     "single email",
			input:    "test@example.com",
			expected: []string{"test@example.com"},
		},
		{
			name:     "multiple emails",
			input:    "test1@example.com test2@example.com",
			expected: []string{"test1@example.com", "test2@example.com"},
		},
		{
			name:     "emails with text",
			input:    "Contact us at test@example.com or support@example.com for help",
			expected: []string{"test@example.com", "support@example.com"},
		},
		{
			name:     "invalid emails",
			input:    "invalid@ @invalid invalid@. invalid@domain",
			expected: []string{},
		},
		{
			name:     "emails with special characters",
			input:    "test+label@example.com test.label@example.com",
			expected: []string{"test+label@example.com", "test.label@example.com"},
		},
		{
			name:     "mixed case emails",
			input:    "Test@Example.com test@example.com",
			expected: []string{"test@example.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emailSet := NewEmailSet()
			count := extractEmailsFromOutput(tt.input, emailSet)
			assert.Equal(t, len(tt.expected), count)

			emails := emailSet.GetAll()
			actualEmails := make([]string, len(emails))
			for i, email := range emails {
				actualEmails[i] = strings.ToLower(email)
			}
			expectedEmails := make([]string, len(tt.expected))
			for i, email := range tt.expected {
				expectedEmails[i] = strings.ToLower(email)
			}
			assert.ElementsMatch(t, expectedEmails, actualEmails)
		})
	}
}

func TestWriteOutput(t *testing.T) {
	emails := []string{"test1@example.com", "test2@example.com"}
	tempDir := t.TempDir()

	tests := []struct {
		name     string
		format   string
		filename string
		validate func(t *testing.T, path string)
		wantErr  bool
	}{
		{
			name:     "write json",
			format:   "json",
			filename: "output.json",
			validate: func(t *testing.T, path string) {
				data, err := os.ReadFile(path)
				require.NoError(t, err)

				var result struct {
					Count  int      `json:"count"`
					Emails []string `json:"emails"`
				}
				err = json.Unmarshal(data, &result)
				require.NoError(t, err)

				assert.Equal(t, len(emails), result.Count)
				assert.ElementsMatch(t, emails, result.Emails)
			},
			wantErr: false,
		},
		{
			name:     "write csv",
			format:   "csv",
			filename: "output.csv",
			validate: func(t *testing.T, path string) {
				file, err := os.Open(path)
				require.NoError(t, err)
				defer file.Close()

				reader := csv.NewReader(file)
				records, err := reader.ReadAll()
				require.NoError(t, err)

				assert.Equal(t, len(emails)+1, len(records))
				assert.Equal(t, []string{"email"}, records[0])

				var foundEmails []string
				for _, record := range records[1:] {
					foundEmails = append(foundEmails, record[0])
				}
				assert.ElementsMatch(t, emails, foundEmails)
			},
			wantErr: false,
		},
		{
			name:     "write txt",
			format:   "txt",
			filename: "output.txt",
			validate: func(t *testing.T, path string) {
				data, err := os.ReadFile(path)
				require.NoError(t, err)

				foundEmails := strings.Split(strings.TrimSpace(string(data)), "\n")
				assert.ElementsMatch(t, emails, foundEmails)
			},
			wantErr: false,
		},
		{
			name:     "invalid format",
			format:   "invalid",
			filename: "output.invalid",
			validate: nil,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputPath := filepath.Join(tempDir, tt.filename)
			err := writeOutput(emails, tt.format, outputPath)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.FileExists(t, outputPath)

			if tt.validate != nil {
				tt.validate(t, outputPath)
			}
		})
	}
}

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "valid relative path",
			path:    "output.txt",
			wantErr: false,
		},
		{
			name:    "valid subdirectory path",
			path:    "subdir/output.txt",
			wantErr: false,
		},
		{
			name:    "invalid absolute path",
			path:    "/absolute/path/output.txt",
			wantErr: true,
		},
		{
			name:    "valid test directory path",
			path:    "/tmp/Test123/output.txt",
			wantErr: false,
		},
		{
			name:    "valid go build path",
			path:    "/tmp/go-build123/output.txt",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sanitizePath(tt.path)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestWorkerGracefulShutdown(t *testing.T) {
	logger = zaptest.NewLogger(t)
	defer logger.Sync()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	jobs := make(chan job, 1)
	var wg sync.WaitGroup
	emailSet := NewEmailSet()

	wg.Add(1)
	go worker(ctx, jobs, emailSet, &wg, 1)

	jobs <- job{
		repoURL:      "https://github.com/test/repo",
		token:        "",
		contributors: false,
	}

	<-ctx.Done()

	close(jobs)

	wg.Wait()
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
