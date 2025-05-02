# Git Email Extractor

A tool to extract email addresses from any git repository. Requires Git on your system or use Docker.

## Usage

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

