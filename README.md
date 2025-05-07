# Git Email Extractor

[![Socket Badge](https://socket.dev/api/badge/go/package/github.com/Sudo-Ivan/Git-Email-Extractor?version=v1.1.1)](https://socket.dev/go/package/github.com/Sudo-Ivan/Git-Email-Extractor?version=v1.1.1)

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

## FAQ

**Why Git instead of API?**

Requiring Git makes it easier to support all types of git platforms, including self-hosted ones. APIs also can change and have rate limits.

