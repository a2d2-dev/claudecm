# Tech Stack

## Core Technologies

| Category | Technology | Version | Purpose |
|----------|------------|---------|---------|
| Language | Go | 1.21+ | Primary development language |
| CLI Framework | Cobra | 1.8.0 | Command-line interface structure |
| Interactive UI | Survey v2 | 2.3.7 | TUI prompts and selection |
| Config Format | YAML | gopkg.in/yaml.v3 | Configuration file format |
| Testing | Go testing | stdlib | Unit and integration tests |
| Mocking | testify/mock | 1.9.0 | Test doubles |
| Build Tool | Go build | stdlib | Compilation |
| Release Tool | goreleaser | 1.24.0 | Multi-platform binaries |
| CI/CD | GitHub Actions | N/A | Automated testing and release |
| Linter | golangci-lint | 1.55.2 | Code quality |

## Dependencies

```go
require (
    github.com/spf13/cobra v1.8.0
    github.com/AlecAivazis/survey/v2 v2.3.7
    gopkg.in/yaml.v3 v3.0.1
    github.com/stretchr/testify v1.9.0 // test only
)
```
