# claudecm - Claude Code Environment Manager

`claudecm` is a CLI tool for managing Claude Code environment configurations. Easily switch between multiple API configurations for different environments, providers, or projects.

## Features

- 🚀 **Fast switching** - Change environments in milliseconds
- 🔒 **Secure storage** - Config files stored with restrictive permissions (600)
- 📝 **Human-readable** - YAML configuration format
- 🎨 **Interactive UI** - Beautiful terminal prompts and selection
- 💻 **Cross-platform** - Works on macOS, Linux, and Windows

## Installation

### From Source

```bash
git clone https://github.com/imneov/claudecm
cd claudecm
make install
```

### Using Go

```bash
go install github.com/imneov/claudecm@latest
```

## Quick Start

### 1. Add your first profile

```bash
claudecm add
```

Follow the interactive prompts to enter:
- Profile name (e.g., "anthropic-us")
- API Base URL (e.g., "https://api.anthropic.com")
- Authentication token
- Default model (optional)
- Description (optional)

### 2. List all profiles

```bash
claudecm list
```

### 3. Switch between profiles

```bash
claudecm switch
```

Or switch directly:

```bash
claudecm switch anthropic-us
```

### 4. Export environment variables

```bash
eval $(claudecm export)
```

This will set the following environment variables:
- `ANTHROPIC_BASE_URL`
- `ANTHROPIC_AUTH_TOKEN`
- `ANTHROPIC_MODEL` (if configured)
- Any custom environment variables

### 5. Delete a profile

```bash
claudecm delete
```

## Commands

| Command | Description |
|---------|-------------|
| `claudecm add [name]` | Add a new profile |
| `claudecm list` | List all profiles |
| `claudecm switch [name]` | Switch active profile |
| `claudecm export` | Export environment variables |
| `claudecm delete [name]` | Delete a profile |
| `claudecm version` | Show version information |
| `claudecm help` | Show help |

## Configuration

Configurations are stored in `~/.claudecm/`:

```
~/.claudecm/
├── profiles/
│   ├── anthropic-us.yaml
│   ├── anthropic-cn.yaml
│   └── moonshot-dev.yaml
└── state.yaml
```

### Profile File Format

```yaml
name: anthropic-us
base_url: https://api.anthropic.com
auth_token: sk-ant-api03-xxxxx
model: claude-sonnet-4
description: "Anthropic US Production API"
custom_env:
  ANTHROPIC_TIMEOUT: "60"
  ANTHROPIC_MAX_RETRIES: "3"
created_at: 2025-10-31T10:30:00Z
updated_at: 2025-10-31T10:30:00Z
```

## Development

### Prerequisites

- Go 1.21+
- Make

### Build

```bash
make build
```

### Run Tests

```bash
make test
```

### Run Linter

```bash
make lint
```

### Development Build (format + vet + test + build)

```bash
make dev-build
```

## Architecture

See [docs/architecture.md](docs/architecture.md) for detailed architecture documentation.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

[MIT License](LICENSE)

## Acknowledgments

- Inspired by [kubecm](https://github.com/sunny0826/kubecm)
- Built with [Cobra](https://github.com/spf13/cobra) and [Survey](https://github.com/AlecAivazis/survey)
