# Contributing to WhatsApp Bridge

Thanks for your interest in contributing! This project aims to be a simple, reliable WhatsApp gateway for AI assistants.

## Getting Started

1. Fork the repository
2. Clone your fork: `git clone https://github.com/YOUR-USERNAME/wa_meow.git`
3. Create a branch: `git checkout -b my-feature`
4. Make your changes
5. Test locally: `./run-server.sh`
6. Commit: `git commit -m "Add my feature"`
7. Push: `git push origin my-feature`
8. Open a Pull Request

## Development Setup

### Prerequisites

- Go 1.21 or later
- SQLite3

### Running Locally

```bash
# Start the server
./run-server.sh

# Test the API
curl -X POST localhost:8090/sessions -d '{"user_id": 1}'
curl localhost:8090/sessions/status?user_id=1
```

### Live Reload

Install [air](https://github.com/air-verse/air) for automatic recompilation:

```bash
go install github.com/air-verse/air@latest
./run-server.sh  # Uses air automatically if available
```

## Code Style

- Follow standard Go conventions
- Run `go fmt` before committing
- Keep functions small and focused
- Add comments for non-obvious logic

## What We're Looking For

### Good Contributions

- Bug fixes with test cases
- Documentation improvements
- Performance optimizations
- New API endpoints that follow existing patterns
- Security improvements

### Out of Scope

- Features requiring complex WhatsApp protocol changes (handled by whatsmeow)
- Authentication/authorization layers (should be handled by reverse proxy)
- Message storage/history (keep the bridge stateless where possible)

## Reporting Issues

When filing a bug report, please include:

1. What you expected to happen
2. What actually happened
3. Steps to reproduce
4. Your environment (OS, Go version, Docker version if applicable)

## Pull Request Guidelines

- Keep PRs focused on a single change
- Update documentation if needed
- Add tests for new functionality
- Ensure all existing tests pass

## Questions?

Open an issue with the `question` label.

## License

By contributing, you agree that your contributions will be licensed under the MIT License.
