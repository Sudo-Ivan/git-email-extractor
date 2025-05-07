# Git Email Extractor

[![Socket Badge](https://socket.dev/api/badge/go/package/github.com/Sudo-Ivan/Git-Email-Extractor?version=v1.1.1)](https://socket.dev/go/package/github.com/Sudo-Ivan/Git-Email-Extractor)

A tool to extract email addresses from any git repository. Only dependency is Git on your system. It can process multiple repositories in parallel.

You can find built binaries for mostly every platform in the [Releases](https://github.com/Sudo-Ivan/Git-Email-Extractor/releases) page. Binaries are built using Github Actions.

### Using Go
```bash
go install github.com/Sudo-Ivan/Git-Email-Extractor@latest
git-email-extractor -t <token> -w <workers> -c <contributors> -f <format> -o <output> <repo-url1> [repo-url2 ...]
```

### Using Docker
```bash
docker run --rm -v $(pwd)/output:/output ghcr.io/sudo-ivan/git-email-extractor:latest -c https://github.com/user/repo
```

### Using Docker Compose
```bash
# Extract emails from a repository
docker-compose run --rm git-email-extractor -c https://github.com/user/repo

# Extract emails and save to output file
docker-compose run --rm git-email-extractor -c -f json -o /output/results.json https://github.com/user/repo
```

## Arguments

The following command-line arguments are available:

*   `-t <token>`: GitHub/GitLab token for private repositories.
*   `-w <workers>`: Number of worker goroutines (default: 4).
*   `-c`: Include contributor emails (committers and signed-off-by).
*   `-f <format>`: Output format (json, csv, txt) (default: "txt").
*   `-o <output>`: Output file (default: stdout).
*   `-config <config-file>`: Path to a configuration file. This file should contain a list of filenames (one per line) to scan for emails within the repositories. If not provided, a default list of common configuration files is used.

## Config Scanner

The tool includes a config scanner that can extract email addresses from common configuration files in git repositories. It supports:

- `pyproject.toml` - Python project configuration
- `setup.py` - Python package setup
- `Cargo.toml` - Rust package configuration

The scanner automatically detects and processes these files in the repository history, extracting email addresses from:
- Author fields
- Maintainer fields
- Contributor lists
- Package metadata

This is particularly useful for finding contact information in project configuration files. The list of files scanned can be customized using the `-config` argument.

## FAQ

**Why Git instead of API?**

Requiring Git makes it easier to support all types of git platforms, including self-hosted ones. APIs also can change and have rate limits.

